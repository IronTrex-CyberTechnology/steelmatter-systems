# R0 engine (I03–I06)

Status: **implemented, developer-tested; awaiting review and independent QA reproduction.** No acceptance case is marked PASS until QA confirms each mapping in `ci/required.json` (`complete: true`).

## Endpoints implemented
| Endpoint | Principal | Notes |
|---|---|---|
| `POST /v1/mandates` | owner | Revision 1, control epoch 1. Signed `SM-MANDATE+JWS`. Buckets start at available = limit. |
| `POST /v1/mandates/{id}/revoke` | owner (the mandate's own) | Locks the mandate, increments the epoch, releases unused reservations and records a `mandate.revoked` outbox event in the same transaction. Repeating it returns 200 with the same state and no second event. |
| `POST /v1/instructions` | entitled issuer | 201 new / 200 identical / 409 `SOURCE_CONFLICT`. The actor comes from `workflow_assignments`. Lines are normalised before authorisation. `client_request_id` is correlation only. |
| `POST /v1/instructions/{id}/revisions` | entitled issuer | 201 appends revision + 1 and releases any unused reservation once. 200 for an exact `amendment_id` retry. 409 for `REVISION_STALE`, `AMENDMENT_ID_CONFLICT` or `ALREADY_COMMITTED`. |
| `POST /v1/actions/evaluate` | assigned actor | Transaction 1 reserves and stores an unsigned issuance. The permit is signed outside any transaction. Transaction 2 relocks and rechecks, then publishes the signature. Returns 201 new / 200 identical retry / 200 DENY. A retry whose live permit was signed by a now-retired key releases it as `KEY_RETIRED` and returns a new permit (201, [ADR-004](adr/ADR-004-key-retirement-and-permit-replacement.md)). |
| `POST /v1/operations` | assigned actor | Verifies the JWS and the holder, audience and digest before any lock. After the locks it rechecks everything, reading time fresh. One transaction then writes the order, lines, obligation, operation, consumption, ledger move and outbox event. 201 new / 200 stored identical result. |
| `POST /v1/actions/simulate` | operator | Same checks as evaluate in a READ ONLY transaction; returns `simulation: true` with `PERMIT_WOULD_ISSUE` or `DENY` and a reason. Creates no permit, reservation or target write (T30). |
| `GET /v1/adapters/{id}/assurance` | actor, issuer, owner, observer, operator | Adapter statement: version, supported operation and paths, limitations, test references. Other principal kinds → 403; unknown adapter → 404 ([ADR-002](adr/ADR-002-authorised-readers.md)). |
| `GET /v1/operations/{id}` | same tenant only: the mandate's owner, an issuer currently entitled to the instruction's source workflow, the instruction's assigned actor, any observer, any operator | Four status fields. Anyone else, including any principal of another tenant, gets 404 `NOT_FOUND` exactly as for an unknown ID ([ADR-002](adr/ADR-002-authorised-readers.md)). |

`reconcile` and `evidence` are implemented in I07 (see [evidence.md](evidence.md)). All 11 endpoints in `api/openapi.yaml` are implemented; `TestOpenAPIEveryOperationIsRouted` keeps it that way.

## Workers (in `steelmatter serve`)
Every `STEELMATTER_WORKER_INTERVAL_MS` (default 1000):
- **Recovery** signs the stored payload of unsigned issuances that are older than `STEELMATTER_RECOVERY_AGE_SECONDS` (default 10) and still valid.
- **Cleanup** releases expired unused issuances under the global locks. The release reason is `EXPIRED`, or `RECOVERED_UNUSED` if the issuance was never signed.
- **Evidence** signs pending execution records and records a first observation for each committed operation.
- **Outbox** delivers each event with its original `event_id` to the synthetic consumer (`steelmatter.event_receipts`), which deduplicates.

## Configuration
`STEELMATTER_DATABASE_URL` (executor role), `STEELMATTER_OBSERVER_URL` (observer role), `STEELMATTER_HTTP_ADDR`, `STEELMATTER_KEYS_DIR` (trust store plus private keys; one signer per JWS `typ`), `STEELMATTER_PERMIT_TTL_SECONDS` (≤ 300), `STEELMATTER_RATE_LIMIT_PER_MINUTE` and `STEELMATTER_RATE_LIMIT_BURST` (per-principal quota, default 1200/min with a burst of 200; `0` disables it). Principals: `STEELMATTER_DB_ADMIN_URL=… steelmatter principal-add -tenant T -id ID -kind ACTOR` prints a token once; only its SHA-256 is stored.

## Failure handling
- **Error classes.** Connectivity, timeouts and cancellation return 503 `SERVICE_UNAVAILABLE` (retryable). Any other unexpected failure, such as a database constraint, returns 500 `INTERNAL_ERROR` (not retryable), logged with SQLSTATE and constraint.
- **Current entitlement.** Evaluate and admit re-read the trusted assignment under the instruction lock; a disabled or changed assignment returns `ACTOR_MISMATCH`.
- **Retries.** Serialization failures and deadlocks are retried, 3 attempts in total with 10 ms and 50 ms backoff, using the same business IDs. See I02 §8 for the full rule and tests.
- **Limits.** Each transaction is bounded by `transaction_timeout` (30 s), each `inTx` call by a 30 s retry budget, and each request by a 60 s deadline.
- Serialization failures and deadlocks are retried up to 3 attempts in total with the same business IDs, then the call returns `RETRIES_EXHAUSTED` with the `operation_id`.
- Lost connectivity returns 503 `SERVICE_UNAVAILABLE`; nothing is allowed from a cache.
- An app-vs-database clock skew above 2 s returns 503 `CLOCK_UNHEALTHY`.
- **Quota.** An authenticated principal over its quota gets 429 `RATE_LIMITED` (retryable, with `Retry-After`) before the body is read, so the request has no effect. The limiter is in-process (R0 is one instance). The default numbers are local placeholders; the packet does not specify a quota. Test: `TestQuotaReturns429WithoutEffect`.
- Mutation transactions set a 30 s statement and idle timeout.

## Failpoints
Named points live in `core/failpoint`. They are compiled in only with `-tags failpoints`; `scripts/package` builds without them, and a unit test asserts they are no-ops there. In-process tests register pause/error actions. Crash tests run the real binary with `STEELMATTER_FAILPOINTS=name=exit`, which exits with status 137.

## Finding fixed during testing
`TestT28AdmissionHoldsLocksAcrossExpiry` caught admission reading permit expiry at lock time. A stall after locking could therefore commit an order on a permit that had just expired. The fix:
- expiry is re-read immediately before the writes;
- the consuming `UPDATE` is guarded with `clock_timestamp() < expires_at`, so zero affected rows aborts the transaction;
- signature persistence is guarded the same way.

## Tests
`tests/integration` contains 40 scenarios against the real HTTP handlers and PostgreSQL. Races use failpoint barriers plus `pg_stat_activity` lock-wait detection, not sleeps. T14, T15 and T16 kill and restart the real binary. The suite was run 5 times in a row with no failures. See the `ci/required.json` case mappings for which tests cover which T-case.
