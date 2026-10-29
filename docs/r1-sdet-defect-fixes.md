# R1 engine fixes for the SDET defect report (29 Oct 2026)

Sindhu's R1 defect report tested `2ec2d8a`. This change fixes the engine-side
findings that need no owner decision. Each fix has a regression test named after
its defect ID. Most are in `tests/integration/r1_defect_fixes_test.go`. Real-Odoo
checks for DEF-01, DEF-14 and DEF-20 are in `tests/integration/r1_odoo_e2e_test.go`.

DEF-04, DEF-08 and DEF-40 waited for owner decisions; they are implemented with
Aakhil's decisions of 29 Oct 2026 in [r1-decisions.md](r1-decisions.md), with the
engine side of DEF-11, DEF-21 and DEF-22 (D1 v1.4). DEF-10, 16, 18 and 19 are in
the Odoo module and connector (Kaif).

| ID | Behaviour now |
|---|---|
| R1-DEF-01 | After a revoke, or when the mandate's control epoch changes, a PENDING operation is only looked up and fenced. Nothing new is ever sent, from the revoke path or from the settle worker. An unsent order is fenced and its amount released. An order that already reached Odoo is found and committed. |
| R1-DEF-03 | With a destination that owns supplier revisions (Odoo), the engine reads the revision from Odoo before it creates or amends an instruction and before evaluation. It copies the revision through `target.sync_supplier_revision` (migration 0016). The copy only moves forward: a revision written backwards in Odoo never makes an old authorisation current again. The executor still has no direct write on `target.suppliers`. |
| R1-DEF-06 | The engine appends a `DISPATCHED` outcome to the anchor right after each dispatch commit. Some intents have no engine row, no `DISPATCHED` marker and nothing at the destination: their engine commit failed, so they are not a restore. The restore check lists them as `unconfirmed` and keeps writes on. Reconciliation never consumes their permit. The client can retry the same dispatch, which reuses the recorded intent, so the anchor append is an idempotent repeat. A restore that rolls back a committed dispatch is still detected. |
| R1-DEF-09 | A FENCED reply, or a refusal reply that claims no earlier create, no longer releases money on its own. The engine reads the anchor's destination stream with its own identity. If that stream shows a create attempt, the operation is HELD. If there is no BLOCK record, or the stream cannot be read, the operation stays UNRESOLVED. |
| R1-DEF-12 | Dispatch with no destination connector configured returns 503 and changes nothing: no dispatch row, no consumed permit, no bucket move. |
| R1-DEF-14 | The anchor refuses a new `CREATE_INTENT` while engine writes are off. The check is ordered against the write switch. The module writes `CREATE_INTENT` before its create commits, so a create that is already past the engine's own check fails closed in Odoo. BLOCK and CREATE_DONE are never refused. |
| R1-DEF-15 | When the recovery anchor or the destination is unreachable, `check`, `reconcile`, `resume` and `settle` return 503 SERVICE_UNAVAILABLE (retryable). They no longer return a non-retryable 500. |
| R1-DEF-20 | An order deleted in Odoo shows up in Odoo's journal as `po_deleted`, so it is not treated as a restore. Its operation is flagged `ORDER_DELETED_AT_DESTINATION` (migration 0015) and stays committed with no refund, and writes stay on. A HELD finding can be recorded against it. If writes are later turned off for another reason, the resume gate asks for a finding on each such operation. |
| R1-DEF-30 | Reconciliation keeps 10 seconds back before its request deadline. If it runs out of time, it stops between operations and returns `complete: false`. Running it again continues from there. The timeout is documented from slow-anchor measurements in [r1-recovery-timeout.md](r1-recovery-timeout.md) (decision 5). |
| R1-OBS-01 | When reconciliation cannot rebuild an operation, the problem now names the cause, for example `PERMIT_MISSING` (the engine was restored to before its permit was issued) or `INSUFFICIENT_BUDGET`. It no longer says INTERNAL_ERROR. |
| R1-P4-DEF-01 | Reconciliation and the resume gate take an engine-wide lock and run one at a time across processes. Each run reports only what it did: one rebuild, one settle and one resume. |
| R1-API-06 | Operator-only endpoints check the caller's role before any configuration check. A non-operator always gets 403. |
| R1-API-01/05/10 | OpenAPI now says 503 for dispatch while writes are off. It drops 409 from reconcile and resume, which never return it. It pins `settled` to `op_ref=STATUS`. It adds the new report fields: `flagged` and `unconfirmed` on check, `unconfirmed` and `complete` on reconcile. Contract tests keep the schemas equal to the engine types. |
| R1-ENV-01 | `scripts/dev-postgres` prints only the export line on stdout, so `eval "$(scripts/dev-postgres)"` works. |
| G-CI | T33–T35 are mapped to engine tests in `ci/required.json` and required in the r1-engine stage. A PASS there is developer evidence; SDET acceptance is separate. |

## Upgrading an existing anchor

The DEF-14 refusal lives in the anchor function `anchor.append_destination`. New
anchors (`anchor-init`, a fresh `destinations/odoo/sandbox/up`) get it
automatically. Upgrade an existing anchor in place once (records untouched, safe
to repeat):

    STEELMATTER_ANCHOR_ADMIN_URL=<anchor owner URL from .local/odoo-anchor.env> steelmatter anchor-upgrade

Migrations 0015 and 0016 run with the normal `steelmatter migrate`.
