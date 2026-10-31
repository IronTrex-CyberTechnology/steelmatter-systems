# R1 recovery timeout (decision 5, R1-DEF-30)

Recovery is resumable rather than bounded by one long call. This note gives
the numbers from the slow-anchor tests and the timeout that follows from them.

## How a reconciliation call spends its time

- Every HTTP request has a 60 s deadline (`server.RequestTimeout`).
- `POST /v1/recovery/reconcile` keeps the last 10 s back (`reconcileReserve`)
  and checks its budget between operations. It never stops inside one, so
  every operation it reports is fully recorded in the engine and the anchor.
- When the budget is spent it returns `complete: false`. The next call reads
  the stores again and continues with what is left. Repeating a call never
  rebuilds or settles an operation twice.
- Rebuilt operations keep their amount held as unresolved until they are
  settled; operations not yet rebuilt keep their reservation. Nothing is
  released because a call stopped.

## Measurements

`tests/integration/r1_slow_anchor_test.go` adds a fixed delay to every round
trip on the engine's anchor connection, with the stub destination and real
PostgreSQL. With a 20 ms delay per anchor round trip:

| Measurement | Result |
|---|---|
| Restore check (3 committed operations) | 7 anchor round trips, about 150 ms |
| Reconciliation, 24 operations lost by an engine restore | about 3.9 anchor round trips per operation, including the start-up reads; about 90 ms per operation |
| Calls needed with 1.2 s of work per call | 2; no operation rebuilt twice; held and reserved amounts correct after each call |

So one rebuilt and settled operation costs about four anchor round trips plus
about 10–15 ms of engine and destination work. Against Odoo, add the Odoo
lookup and fence round trips for each operation.

## Timeout

- **Per call:** keep the 60 s request deadline. A call does about 50 s of
  work: roughly `50 s / (4 × anchor round trip + destination time)`
  operations. At a 50 ms anchor round trip and 100 ms of Odoo time per
  operation that is about 160 operations per call.
- **Client and operator retries:** call reconcile again while it returns
  `complete: false` (it is safe to repeat, and the SDK retries 503). The resume
  gate stays closed until reconciliation is complete and every check passes.
- **Anchor unreachable:** check, reconcile, resume and settle return 503
  (retryable). Nothing moves; repeat when the anchor is back.
- **Odoo module side:** the module's anchor statement timeout
  (`STEELMATTER_ANCHOR_TIMEOUT_MS`, sandbox 2000 ms) bounds one create or
  fence. A create whose anchor write times out rolls back and is reported as
  unavailable, never as "not sent".

Re-run the measurements on the deployment network before go-live and adjust
the per-call estimate; the resumable behaviour does not depend on them.
