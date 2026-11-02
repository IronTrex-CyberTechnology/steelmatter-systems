# R1 recovery: state transitions and operator procedures

Use this procedure after an engine database restore. While recovery is active, issuers cannot create or amend instructions. Recovery can supersede an instruction automatically only when the records prove it was never dispatched or executed. [Recovery rules](r1-decisions.md) · [Regression tests](r1-sdet-defect-fixes.md)

## 1. States

| State | Writes | Entered by | Left by |
|---|---|---|---|
| NORMAL | on | resume gate passed | any trigger below |
| RECOVERY | off (new generation) | `POST /v1/recovery/check` finds a signal; startup check; the admission barrier (section 2); an anchor clock difference above 2 s; an unreadable comparison | `POST /v1/recovery/resume` passes every gate check |

While writes are off: create, amend, evaluate and dispatch return 503
(retryable); `check`, `reconcile`, `resume`, `settle` and the operator endpoints
work; nothing is sent to Odoo except lookups and fences (never in OBSERVE_ONLY).
Held and committed amounts are never released without proof.

Standard path: `check` (writes go off if a signal is found) → `reconcile`
(rebuild from the anchor, settle by lookup, never create an order) → resolve each entry in
`problems` using section 4 → `resume` (the gate checks run; writes come back only if all
pass).

## 2. The window before detection

A restore is not known until a check runs. Before this change, an issuer could
use that window to re-create or re-amend what the restore had erased
(NEW-RTFC79-01, NEW-RTFC79-02). Odoo deduplicates only by `op_ref`, so a new
instruction for the same ERP record produces a second purchase order.

Now every create, amend, evaluate and dispatch reads the recovery anchor first
(`guardSourceHistory`) and refuses, writing nothing, when the anchor shows:

1. a dispatch for the same source record (system and record ID) under a
   different instruction, whatever the mandate; or
2. a dispatch for this instruction under another operation that the engine
   database lacks (and that is not an unsent dispatch whose engine commit never
   happened, R1-DEF-06).

The refusal is 503 `RESTORE_SUSPECTED` (retryable), turns writes off, and leaves
every amount as it is. Recovery then runs the standard path, which rebuilds the
anchored instruction before any issuer change can compete with it. After resume
the same request is refused normally (409 `SOURCE_CONFLICT`, or the instruction
is committed).
Tests: `TestRTFC7901NoSecondPurchaseForTheSameSourceRecord` (create, evaluate,
dispatch), `TestRTFC7902AmendCannotReuseAnchoredRevisionNumber`.

Role and tenant checks come first: a caller who is not an entitled issuer gets 403
(404 for an instruction outside its source systems, 404 for a mandate outside its
tenant) whatever the recovery state is, and nothing about recovery appears in
those answers (`TestRTFC79RoleChecksComeBeforeRecoveryState`,
`TestRT913SEC01OtherTenantLearnsNothingAboutRecovery`).

Limit: a DISPATCH_INTENT written before commit 0ee221a carries no source key, so
the source-record check cannot see it (the instruction check still can), and it
cannot be used to rebuild a lost instruction (`INSTRUCTION_MISSING`).

## 3. Automatic or operator review (rule 7)

| What recovery finds | Result |
|---|---|
| Instruction or revision made after the restore that never spent: no dispatch in any status, no operation, no consumed permit, no committed revision, no other dispatch intent in the anchor | Superseded automatically. Rows are kept; open reservations are released (`RECOVERED_UNUSED`); the replacement is recorded as an outbox event (`instruction.superseded_by_recovery`, `instruction.revision_restored_by_recovery`). The anchored one is rebuilt; its order is confirmed by lookup. |
| The same, but dispatched, spent or uncertain (`CONFLICT_SPENT`) | Nothing is superseded, released or sent. Reconciliation reports the problem, the gate stays closed. An operator resolves it (section 4, B). |
| The anchored revision number is already used by a different action (`REVISION_NUMBER_TAKEN`) | Nothing is changed or overwritten. The gate stays closed. See section 4, C. |
| Mandate, permit or action does not verify, or the anchor's intent predates the source fields | Named cause (`MANDATE_MISSING`, `PERMIT_INVALID`, `ACTION_DIFFERS`, `INSTRUCTION_MISSING` ...), nothing rebuilt, gate closed (section 4, D). |

Recovery never creates an order, never restores a revoked mandate or permission,
and never releases an amount that spent or may have spent.

## 4. Operator procedures

### A. Restore detected

1. `POST /v1/recovery/check`: writes go off and the signals are listed.
2. `POST /v1/recovery/reconcile`: repeat until `complete` is true and `problems`
   is empty (safe to repeat; one run at a time across processes).
3. For every entry in `problems`, follow the matching procedure below.
4. `POST /v1/recovery/resume`: read `checks`; fix the ones that failed and try again.

### B. `CONFLICT_SPENT`: two instructions, two dispatches, one source record

Cause: an instruction made after the restore for the same source record
already spent, so the anchored operation could not be rebuilt. Current builds
prevent this through the admission barrier; it remains possible for state made
by an older build or a process that does not consult the anchor.

1. Do not resume writes. Do not cancel anything yet.
2. In Odoo, open the two purchase orders for the source record (the anchored
   `op_ref` and the conflicting one). Decide with purchasing what happens to the
   duplicate (cancel, or accept). This is a business action in Odoo; the engine
   does not create, edit or cancel orders.
3. As an operator: `POST /v1/recovery/conflicts/{op_ref}/adopt` for the
   anchored `op_ref`, with `evidence_ref` (ticket or document) and `note`.
   It works only while writes are off.
   - The anchored operation is recorded again with its amount held; the source
     record is given to the anchored instruction; every row, order and held or
     committed amount of the other instruction is kept.
   - A `SOURCE_CONFLICT_ADOPTED` finding is recorded in the anchor for every
     operation of the source record, with your evidence and name.
   - Nothing is created, cancelled or released. A repeat changes nothing.
4. `POST /v1/recovery/reconcile` settles the rebuilt operation by lookup
   (committed if its order exists). If purchasing cancelled one order in Odoo,
   the engine flags it `ORDER_CHANGED_AT_DESTINATION` (R1-DEF-13); record a
   finding for it.
5. `POST /v1/recovery/resume`. The check `SOURCE_RECORDS_UNIQUE` fails until
   every operation of the source record has the finding.

Test: `TestRT0EESEC01DispatchedConflictStopsForReview` (committed and uncertain).

### C. `REVISION_NUMBER_TAKEN`

Cause: the instruction was amended after the restore to a different action under
a revision number the anchor shows was dispatched. Current builds refuse that
amendment before the number is used (section 2), so this is reachable only for
state made by an older build.

The revision number already identifies another immutable action. The engine cannot rebuild the anchored operation, so its amount is absent from the engine’s balance and the resume gate stays closed. R1 has no supported repair for this historical conflict. Keep writes disabled, preserve the conflicting revision and anchor evidence, record an operator finding and escalate to the deployment owner.

### D. Other named causes

- `MANDATE_MISSING`: restore or re-provision the mandate (owner), then reconcile.
- `INSTRUCTION_MISSING` with "predates the source fields": the anchor's intent was
  written before 0ee221a and cannot rebuild the instruction. Escalate; do not
  recreate it by hand under a new ID (that is a second purchase).
- `INSUFFICIENT_BUDGET`: the mandate no longer has the amount free; review the
  mandate's other holds before anything is released.
- `PERMIT_*` / `ACTION_DIFFERS` / `MANDATE_DIFFERS`: the anchor and the engine
  disagree on signed content. Escalate; nothing was changed.

## 5. Repeated and concurrent recovery

Reconcile and resume take an engine-wide lock and run one at a time. A repeat
rebuilds nothing twice and reports only its own work. Each rebuilt operation is
confirmed by lookup before any amount moves out of held.

## 6. API behaviour during recovery

| Call | During recovery |
|---|---|
| `POST /v1/instructions` | 403 if the caller is not an entitled issuer; 404 if the mandate is not in the caller's tenant; otherwise 503 `RECOVERY_IN_PROGRESS` while writes are off; 503 `RESTORE_SUSPECTED` if the anchor shows the source record dispatched. |
| `POST /v1/instructions/{id}/revisions` | 403 not an issuer; 404 outside the issuer's source systems; 503 while writes are off or `RESTORE_SUSPECTED`; an exact retry of a recorded amendment is 200 with `status` and `effective`, except 409 `AMENDMENT_SUPERSEDED` when recovery restored an older revision; 409 `INSTRUCTION_SUPERSEDED` for a superseded instruction. |
| `GET /v1/instructions/{id}` | `status` is `OPEN`, `COMMITTED` or `SUPERSEDED`; `superseded_by` names the instruction recovery kept. |
| `POST /v1/actions/evaluate`, `POST /v1/dispatches` | 503 while writes are off or `RESTORE_SUSPECTED`; 409 `INSTRUCTION_SUPERSEDED`. |

## 7. Recovery limits

- The engine checks the source record. The Odoo module deduplicates by `op_ref`.
- `REVISION_NUMBER_TAKEN` resolution (section 4, C).
- The anchor query by source key scans the intent records. Its cost grows with the history retained.
