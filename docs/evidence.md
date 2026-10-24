# Evidence, observer and verifier (I07)

Status: **implemented and developer-tested; awaiting review and QA reproduction.** The observer shares host and database administrators with the executor, so an observation is a separated readback, **not** independent third-party attestation (document 02).

## Observer
`core/observer` connects with the **observer database role**. Its complete read set is `target.suppliers`, `target.purchase_orders`, `target.purchase_order_lines`, `steelmatter.action_revisions`, `steelmatter.operations` and `steelmatter.obligations`. It has no other reads (not the ledger, principals, observations, event receipts or schema metadata) and no writes. Migration 0006 revoked three extra reads granted earlier (QA-D1-001). `TestObserverExactReadSetFreshDatabase` and `TestObserverExactReadSetAfterUpgrade` pin the exact set. It signs with a key whose only purpose is `SM-OBSERVATION+JWS`.

**Projection (pinned):**
- The observed action is the expected action with every **material** field replaced by its readback value: `supplier_id`, `supplier_revision`, `delivery_id`, `currency`, `lines` (sorted by item_id bytes) and `amount_minor`.
- Identity fields that the destination does not hold are taken from the stored operation: `schema_version`, `crypto_profile`, `tenant_id`, `instruction_id`, `action_revision`, `actor_id`, `audience`, `operation`.

| Readback | Status | `observed_action_digest` |
|---|---|---|
| Complete, digest equals the expected digest | `MATCH` | equal to expected |
| Complete, digest differs | `MISMATCH` | the observed digest |
| Order, lines or any material field missing | `INSUFFICIENT_EVIDENCE` | `null` |

A database `CHECK` enforces the same rules, so missing fields can never be stored as `MATCH`. The service stores the observer-signed record (`steelmatter.observations`) separately from the executor's execution record (I9).

## Signed records and the export
- **Execution record:** canonical bytes and digest are stored in the commit transaction. The record is signed (`SM-EXECUTION+JWS`) right after commit, with a worker as backstop, so evidence can be `PENDING` briefly. The stored bytes never change.
- **`POST /v1/operations/{id}/reconcile`** (observer or operator of the operation's tenant; everyone else gets 404): a read-only readback now, returning 200 `RESOLVED`. It never creates or repairs orders. An operation that never committed returns `NOT_OBSERVED`.
- **`GET /v1/operations/{id}/evidence`** returns 200 only to a principal of the operation's tenant that is the mandate's owner, an issuer currently entitled to the instruction's source workflow (not necessarily the issuer that created it), the instruction's assigned actor, any observer or any operator. Everyone else gets 404 `NOT_FOUND`, the same response as for an unknown operation, so existence is not disclosed ([ADR-002](adr/ADR-002-authorised-readers.md)). It returns:
  - the mandate, permit, execution and latest observation JWS, exactly as issued;
  - the action;
  - the service's link claims;
  - `incomplete`, listing missing records.

  It contains no secrets and no trust anchors.
- **Evidence status:** stored records are re-verified whenever they are read. It is `INVALID` if any present record fails verification, or if a verified record does not match the digests the service stored for this operation or does not link to the other records (review F-10), or if the records' timestamps are impossible together (ADR-003: for example committed after the permit expired, or observed before the commit). Otherwise it is `AVAILABLE` once the execution record is signed, and `PENDING` before that.
- **Separation checks at startup (review F-09):** the observer must connect as a different database role from the executor, and that role must be unable to write any Steelmatter table. In the service's signing keyring only, the observation key must have no other purpose and is never given to the engine. Trust stores used for verification may combine purposes (CLR-02).

## Standalone verifier (`packages/verifier`)
`verifyBundle(bundle, trustStore, now?)` and the CLI need no engine, database, model API or network. Trust anchors must be supplied explicitly. The report keeps every dimension separate and has **no overall verdict**:

| Dimension | Values |
|---|---|
| Per record: `integrity` | `VALID`, `INVALID`, `UNVERIFIED` (untrusted key), `MISSING` |
| Per record: `issuer` | `TRUSTED`, `UNTRUSTED`, `UNKNOWN` |
| Per record: `profile`, `payload` | `SUPPORTED`/`UNSUPPORTED`; `VALID`/`INVALID` |
| `usability` | permit `USABLE`/`EXPIRED`/`NOT_YET_VALID`; mandate window. Time only; consumption and revocation state are known only to the service. |
| `linkage` | `COMPLETE`, `INCOMPLETE` (a record is missing or unverified), `BROKEN` (any mismatch). Named checks recomputed from the signed payloads, including whether the service's link claims are true and five cross-record time checks (`time:*`, ADR-003). |
| `chronology` | `CONSISTENT`, `INCONSISTENT` (a cross-record time rule is violated), `INCOMPLETE` (records missing). The five `time:*` checks (ADR-003), also counted in `linkage`. Report version 2. |
| `observation` | The observer's status, plus `observed_matches_action` recomputed against the bundled action |

```bash
node packages/verifier/src/cli.ts verify --trust trust-store.json --bundle evidence.json --format text
```
Exit code is 0 once a report is produced, and 2 for usage or input errors. Pass `--fail-on integrity,issuer,profile,linkage,chronology,observation,usability` to make the named dimensions gate the exit code (1 if any is not clean).

Unknown fields in the bundle envelope, `records` or `links` are rejected with `UNKNOWN_FIELD`.
The library throws `ContractError`; the CLI exits 2 without producing a report. This prevents
unverified additions from travelling inside an otherwise accepted bundle. Missing known records
still produce an incomplete report, and signed payloads retain their existing schema and signature
checks. The service's status claims remain claims, not proof.

Fixture: `fixtures/vectors/evidence-v1.json`, generated by `go run ./cmd/smvectors -only evidence`. It holds a Go-signed complete chain plus broken-link, observer-mismatch and incomplete variants. `fixtures/vectors/evidence-temporal-v1.json` (`-only temporal`) holds validly signed chains that differ only in timestamps, with the exact time checks each must fail.

## Bugs found during testing
- **Observation ordering.** Several observations recorded within one second could tie, because signed timestamps are whole seconds, so "latest" was ambiguous. A full-precision `recorded_at` column now orders them.
- **Evidence fixture.** The fixture generator had built the broken-link case with a false service link claim. The verifier correctly flagged both problems; the fixture now isolates the single intended break.
