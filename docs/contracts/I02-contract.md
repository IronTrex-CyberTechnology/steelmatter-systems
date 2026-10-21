# I02 contract: payloads, signing, API and locking

Baseline 0.6 (packet 0.6.2) · Engine in Go ([ADR-001](../adr/ADR-001-go-service.md)) · Status: **reviewed and adopted for implementation**

This document pins the behaviour the I02 checklist asks for. Each section names the code and tests that implement it. Results here are **contract and prototype checks**: they do not mark any T-case PASS (see §9).

## 1. Pinned versions

| Component | Version | Where |
|---|---|---|
| Go / PostgreSQL / Node | 1.27.3 / 17.10 / 26.11.0 | `versions.env` |
| pgx | v5.11.0 | `go.mod` |
| JWS (Go) | `github.com/go-jose/go-jose/v4` v4.1.5 | `go.mod` |
| RFC 8785 (Go) | `github.com/gowebpki/jcs` v1.0.2 | `go.mod` |
| JSON Schema 2020-12 (Go) | `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3 | `go.mod` |
| YAML (OpenAPI tests only) | `go.yaml.in/yaml/v3` v3.0.5 | `go.mod` |
| JWS (TS) | `jose` 6.2.12 | `packages/verifier/package-lock.json` |
| RFC 8785 (TS) | `canonicalize` 5.1.0 | same |
| JSON Schema (TS) | `ajv` 8.20.0, `ajv-formats` 3.0.1 | same |

npm installs run with lifecycle scripts disabled (`packages/verifier/.npmrc`). Every library was at least three weeks old when pinned.

## 2. Strict JSON input profile
Implemented in `core/contract/strictjson.go` and `packages/verifier/src/strict-json.ts`. Each is a hand-written recursive-descent parser, so both runtimes enforce identical rules. Neither `encoding/json` nor `JSON.parse` rejects duplicate keys or keeps integers exact.

| Rule | Code |
|---|---|
| Body > 1 MiB | `PAYLOAD_TOO_LARGE` |
| Invalid UTF-8, byte-order mark, unpaired `\uD800`–`\uDFFF` escape | `INVALID_ENCODING` |
| Syntax error, leading zeros, `NaN`, trailing comma or content, depth > 32 | `INVALID_JSON` |
| Repeated object key (compared after unescaping, so `"a"` and `"\u0061"` collide) | `DUPLICATE_KEY` |
| Fraction, exponent or `-0` (including `1.0` and `6e7`) | `NON_INTEGER_NUMBER` |
| Integer outside ±(2^53−1) | `NUMBER_OUT_OF_RANGE` |

**Nesting depth (CLR-01).** Every JSON value (object, array, string, number, boolean or null) has a depth. The top-level value has depth **0**, and a value directly inside an object or array has the container's depth + 1. Input is rejected with `INVALID_JSON` if any value has depth greater than **32**. Scalars count: a value inside the deepest allowed container is itself one level deeper. Go and TypeScript behave identically at the boundary:

| Input | Deepest value | Result |
|---|---|---|
| 33 nested arrays, innermost empty | innermost array at depth 32 | accept |
| 34 nested arrays, innermost empty | depth 33 | reject |
| 32 nested arrays containing `1` | the `1` at depth 32 | accept |
| 33 nested arrays containing `1` | the `1` at depth 33 | reject |
| `{"a":` + 32 nested empty arrays + `}` | innermost array at depth 32 | accept |
| `{"a":` + 33 nested empty arrays + `}` | depth 33 | reject |
| `{"a":` + 31 nested arrays containing `1` + `}` | the `1` at depth 32 | accept |
| `{"a":` + 32 nested arrays containing `1` + `}` | the `1` at depth 33 | reject |

Real payloads nest at most three levels; the limit only protects the parsers from pathological input. The boundary is pinned by `fixtures/vectors/depth-v1.json` (11 accept/reject cases, including every row above), run by `TestParseStrictDepthBoundary` (Go) and `test/depth.test.ts` (TypeScript).

The first fault in document order wins. The vectors contain one fault each; for inputs with several faults, only rejection is guaranteed, not which code.

## 3. Payload validation
Implemented in `core/contract/payloads.go` and `packages/verifier/src/payloads.ts`. The schemas are the packet schemas, unchanged (`schemas/`). The verifier keeps a byte-identical copy, enforced by `TestConformanceVerifierSchemasMatchEngine`.

Checks run in this pinned order; the first failure decides the code:
1. Strict parse (§2).
2. Top level must be an object → `SCHEMA_VIOLATION`.
3. `schema_version == "1"` → else `UNSUPPORTED_VERSION` (422).
4. `crypto_profile == "SM-JWS-ES256-1"` → else `UNSUPPORTED_PROFILE` (422).
5. No fields outside the schema, including inside lines → `UNKNOWN_FIELD`. This is what rejects a premature `obligation_id` (T39).
6. `currency == "INR"` when present → else `UNSUPPORTED_CURRENCY` (422).
7. Timestamps must be real UTC `YYYY-MM-DDTHH:MM:SSZ` → else `INVALID_TIMESTAMP`. Rejected: Feb 29 in non-leap years, `:60` leap seconds, fractions, offsets, lowercase `z`, hour 24.
8. JSON Schema 2020-12 with format assertion → `SCHEMA_VIOLATION`.
9. Semantic rules:
   - **action:** item IDs must be unique → `DUPLICATE_LINE_ITEM` (checked before order). Lines must be strictly ascending by item_id *bytes* → `LINES_NOT_CANONICAL`. Each `quantity × unit_price_minor` and the running sum must stay ≤ 2^53−1 → `ARITHMETIC_OVERFLOW`. The sum must equal `amount_minor` → `AMOUNT_MISMATCH`.
   - **mandate:** `expires_at > issued_at` → `INVALID_TIME_RANGE`.
   - **permit:** the same check, plus lifetime ≤ 5 minutes → `INVALID_TIME_RANGE`.
   - **observation:** `MATCH` requires an observed digest equal to the expected one. `MISMATCH` requires a non-null, different observed digest. `INSUFFICIENT_EVIDENCE` requires an observed digest of `null`. Otherwise → `OBSERVATION_INCONSISTENT`.

**Proposal normalisation (T40).** Issuer-proposed lines go through `NormaliseLines` *before* authorisation: duplicate item IDs are rejected (never merged), then lines are sorted by item_id bytes. Stored and signed actions must already be canonical; reordering them later is rejected as `LINES_NOT_CANONICAL`, never silently fixed.

## 4. Canonical bytes and digests
- **Canonical bytes:** RFC 8785, applied to the strictly parsed value. Given the §2 profile (strings, safe integers, booleans, null), Go `jcs` and TS `canonicalize` produce identical bytes.
- **Digest:** `hex_lower(SHA-256( ASCII("steelmatter:<kind>:v1") || 0x0A || canonical ))`, where `kind` is one of `mandate`, `action`, `permit`, `execution`, `observation`. The domain separation stops a digest of one payload type from matching another. Unlike a plain `sha256sum`, the prefix must be included. To reproduce a digest with standard tools, given the canonical bytes in `action.json`:

  ```bash
  { printf 'steelmatter:action:v1\n'; cat action.json; } | shasum -a 256
  ```
- **Fields that hold digests:**
  - `action_digest` and `expected_action_digest`/`observed_action_digest` hold action digests.
  - `mandate_digest` holds the mandate digest.
  - `permit_digest` holds the permit digest.
  - `execution_digest` holds the execution digest.

  Each is computed over the payload's canonical bytes, never over the JWS.

## 5. SM-JWS-ES256-1 signing profile
Implemented in `core/contract/jws.go` and `packages/verifier/src/jws.ts`.

- Signed kinds and their `typ` values: `SM-MANDATE+JWS`, `SM-PERMIT+JWS`, `SM-EXECUTION+JWS`, `SM-OBSERVATION+JWS`. Actions are **not** signed; the permit binds `action_digest`.
- The protected header is exactly `{"alg":"ES256","kid":"<kid>","typ":"<typ>"}`: these three members, this order, no whitespace. `kid` is the RFC 7638 thumbprint of the public key.
- The JWS payload is the canonical bytes. The signing input is `B64URL(header) "." B64URL(payload)`, unpadded. The signature is the 64-byte raw `R||S` value.
- Signing uses the libraries (go-jose / jose). Both implementations assert that the library output starts with the pinned signing input.
- **Trust store** (`core/keys`): a local file listing `kid`, public JWK and `purposes`, meaning which `typ` values that key may sign. **Purposes (CLR-03):** a non-empty list of distinct values, each exactly one of `SM-MANDATE+JWS`, `SM-PERMIT+JWS`, `SM-EXECUTION+JWS` or `SM-OBSERVATION+JWS`. Any other value is rejected when the trust store is loaded, in Go and TypeScript, and when a key is generated: unknown types such as `SM-ACTION+JWS`, empty strings, other letter case, surrounding whitespace, non-strings and repeats. There is no remote key fetch; header members such as `jku`, `jwk`, `x5u` and `x5c` are rejected. Trust-store files are parsed strictly in both runtimes: exact field names and exact JSON types (`null` is never accepted for any field or list item), no duplicate members, no repeated `kid` or name, and a valid RFC 3339 `created_at` (real calendar values, optional fractional seconds, `Z` or an offset within ±23:59; QA-D1-003). `fixtures/vectors/truststore-v1.json` (generated by `go run ./cmd/smvectors -only truststore`) holds the accept/reject cases both parsers must agree on (QA-D1-002). Private key files must be mode 0600 or stricter. **Historical trust keys (I02 checklist item 5):** a trust-store key may carry an optional `not_after` (RFC 3339, later than `created_at`), retiring it from that instant. A retired key:
- is refused for signing, at startup (`LoadKeyring`) and at signing time, so evaluate fails with 503 before anything is reserved;
- no longer authorises admission (a permit it signed returns 403 `JWS_KEY_UNTRUSTED`);
- still verifies records it signed **before** `not_after`, which remain valid historical evidence. The verifier reports `key_status: RETIRED` and `signed_within_key_validity: true`.

A record whose own time (mandate/permit `issued_at`, execution `committed_at`, observation `observed_at`) is at or after its key's `not_after` is untrusted, and the service reports its evidence as INVALID. Rotate with `smkeys retire -name … -at …`. A future `not_after` takes effect at that instant without a restart. Tests: `TestHistoricalTrustKeys`, the `not-after-*` parity cases, and the TypeScript historical-key evidence tests.

**Signing keyring only (CLR-02, review F-09):** among the keys a running service holds a private key for (`contract.LoadKeyring`), a key with `SM-OBSERVATION+JWS` must have no other purpose, and the engine never holds it (`contract.SplitObserver`). This does not apply to trust-store import or verification: trust stores, and verification-only keys without a private key, may combine any valid purposes. A record's `typ` is checked against its key's purposes. Bootstrap creates three keys:
  - `steelmatter-service` signs permits and execution records.
  - `steelmatter-observer` signs observations.
  - `synthetic-owner` signs mandates.

Verification check order (pinned):

| # | Check | Code |
|---|---|---|
| 1 | ≤ 1,402,197 bytes; three segments; strict unpadded base64url | `JWS_MALFORMED` |
| 2 | Header parses strictly as an object | `JWS_MALFORMED` |
| 3 | `alg == "ES256"` (rejects `none`, `HS256`, `ES384`, missing) | `JWS_ALG_UNSUPPORTED` |
| 4 | Only `alg`, `kid`, `typ` (rejects `crit`, `jku`, `jwk`, `x5u`, …) | `JWS_HEADER_UNSUPPORTED` |
| 5 | `typ` equals the expected kind | `JWS_TYPE_MISMATCH` |
| 6 | `kid` is trusted *and* allowed to sign this `typ` | `JWS_KEY_UNTRUSTED` |
| 7 | Header bytes equal the pinned form | `JWS_MALFORMED` |
| 8 | ES256 signature verifies (64 bytes; not DER) | `JWS_SIGNATURE_INVALID` |
| 9 | Payload passes §3 for the kind | that payload's code |
| 10 | Payload bytes equal their RFC 8785 form | `JWS_PAYLOAD_NOT_CANONICAL` |

**Signature malleability.** ECDSA signatures are malleable: `(r, s)` and `(r, n−s)` both verify, so the same permit can appear as two different JWS strings. Steelmatter therefore never uses JWS bytes as an identity. Issuances, operations and evidence links use the payload digest. `TestMalleatedPermitSignatureCreatesNoSecondIdentity` admits a re-encoded permit and shows it maps to the same single operation. Neither runtime enforces low-S.

Verification establishes integrity and signer attribution only (I11). Expiry and usability, linkage between records, and observation status are separate results; an expired historical permit still verifies as evidence.

**Cross-record timestamps ([ADR-003](../adr/ADR-003-evidence-temporal-linkage.md), accepted 2026-10-21; closes F-11).** Timestamps are whole UTC seconds from one clock (PostgreSQL `clock_timestamp()`); expiry is exclusive. An evidence chain is temporally consistent only if:

| Linkage check | Rule |
|---|---|
| `time:permit_issued_within_mandate` | mandate.issued_at ≤ permit.issued_at < mandate.expires_at |
| `time:permit_expiry_within_mandate` | permit.expires_at ≤ mandate.expires_at |
| `time:execution_within_permit` | permit.issued_at ≤ execution.committed_at < permit.expires_at |
| `time:execution_within_mandate` | mandate.issued_at ≤ execution.committed_at < mandate.expires_at |
| `time:observation_after_execution` | execution.committed_at ≤ observation.observed_at |

A violated rule is a linkage `MISMATCH`: verifier `chronology: INCONSISTENT` and `linkage: BROKEN` (report version 2; CLI `--fail-on chronology` or `linkage` exits 1); service `evidence: INVALID`. A rule whose records are absent is `MISSING`. Equal seconds are valid where the rule says ≤ (for example observation and execution in the same second). No skew tolerance. Go `contract.TemporalLinkage` and the TypeScript verifier use the same names; `fixtures/vectors/evidence-temporal-v1.json` (`go run ./cmd/smvectors -only temporal`, self-checked against Go) pins them: `TestConformanceTemporalEvidenceVectors`, `packages/verifier/test/temporal.test.ts`, `TestTemporalLinkageRules`, `TestEvidenceTemporalLinkageEnforced`.

## 6. Golden vectors
`fixtures/vectors/golden-v1.json` is generated by `go run ./cmd/smvectors`. The private keys are discarded after generation. The generator self-checks every vector against Go before writing the file.

| Set | Count | Both runtimes must… |
|---|---|---|
| `canonical` | 8 | Produce the same canonical bytes, digest context and digest from deliberately messy input (reversed key order, whitespace, `\u0061` escapes, byte-ordered lines, 2^53−1 bounds) |
| `payload_negative` | 48 | Reject with the exact listed code |
| `jws_positive` | 4 | Match the protected header and signing input, verify, and report the same payload digest |
| `jws_negative` | 36 | Reject with the exact listed code (algorithm attacks, header injection, type confusion, untrusted or wrong-purpose keys, kid substitution, tampering, DER/truncated/zero signatures, encoding, non-canonical payloads) |

Fresh cross-runtime signatures (`TestConformanceCrossRuntimeFreshSignatures`) work like this:
1. Go signs all four signed kinds with a new key.
2. TypeScript verifies them, re-signs the same payloads with its own new key, and reports its signing inputs.
3. Go verifies the TypeScript signatures and checks that signing inputs and digests are equal. Signature bytes are deliberately *not* compared.

Regenerating the vector file changes keys and signatures, so it is a reviewed contract change.

## 7. API contract
[`api/openapi.yaml`](../../api/openapi.yaml) is OpenAPI 3.1 and covers all 11 endpoints from document 03 with closed schemas. The error envelope is `{error_code, message, request_id, retryable, operation_id?}`.

- **Single code list.** `core/httpapi/codes.go` maps each condition to one code, with its HTTP status, retryability and use. Use is one of: `error`, `deny` (carried as the reason in a 200 DENY), or `both` (a DENY reason at evaluation, an error at admission). The OpenAPI `ErrorCode` and `DenyReason` enums are tested to equal it exactly.
- **Retryable** means exactly 429 or 503.
- **Who may call what.** Allowed principal kinds are declared per operation in `x-steelmatter-principals`.
- **Authorised readers** of operation status, evidence and reconcile, and the 404-without-disclosure rule, are defined in [ADR-002](../adr/ADR-002-authorised-readers.md).
- **Server-owned fields are not accepted** in request bodies: `tenant_id`, `actor_id`, `issuer_id`, `action_revision`, `amount_minor` (derived), `supplier_revision` (read from the protected supplier row) and any destination field. Supplying one is rejected as `UNKNOWN_FIELD`. A mismatched actor inside a presented action is rejected as `ACTOR_MISMATCH`.
- **Amendment body:** exactly `amendment_id`, `expected_action_revision`, `proposed_action`.
- **Tests** (`api/openapi_test.go`):
  - the endpoint set is exact;
  - every object schema is closed;
  - every `$ref` resolves;
  - code enums equal the catalogue;
  - size limits equal `core/contract`;
  - server-owned fields are absent from requests.

Each guard was checked by deliberately breaking the spec.

## 8. Tables, constraints and lock order
Migration [`0002_core_schema.sql`](../../core/db/migrations/0002_core_schema.sql).

**Lock order for every mutation:**
1. `mandates`: the control row, which also holds the allowance buckets.
2. `instructions`
3. `action_revisions`
4. `issuances`: reservation and permit in one row.
5. `target.suppliers` (`FOR SHARE`), then target writes.

Rows of one type are locked in primary-key order. All post-lock time checks use `clock_timestamp()`.

**Database backstops:**
- `CHECK` that available + reserved + committed + unresolved = limit, with every bucket ≥ 0.
- At most one live issuance per action revision, and one consumed issuance per instruction.
- One order, obligation and operation per instruction.
- A unique source key `(tenant, source_system, source_record_id)`.
- Permit window ≤ 5 minutes.
- An `ACTIVE` or `CONSUMED` issuance must have a signature.
- An amendment's result revision must equal expected + 1.

**Immutability (migration 0005, review F-17):** the executor has column-level UPDATE only on workflow columns: status, buckets, epoch, delivery time and the write-once signature columns. Canonical payloads, digests, identities, amounts and windows are not updatable. For every role, administrators included, triggers make persisted signatures write-once and enforce the issuance state machine (`UNSIGNED → ACTIVE → CONSUMED`, `UNSIGNED/ACTIVE → RELEASED`, never back). Violations raise SQLSTATE `SM001`.

**Privileges:**
- The executor has SELECT/INSERT on engine tables, UPDATE only on the columns listed above, and no DELETE, TRUNCATE or DDL. On suppliers it may update only `last_admission_at`, which is what makes `FOR SHARE` locks possible without letting it change supplier data.
- The observer has SELECT on the target tables, `action_revisions`, `operations` and `obligations` only. This is enforced since migration 0006 (QA-D1-001) and pinned by an exact read-set regression test on fresh and upgraded databases.

**Isolation:** READ COMMITTED with `SELECT … FOR UPDATE`. Each rechecked row is re-read after its lock is taken. With a single global lock order no deadlock is expected. **Retries (specified):** only SQLSTATE `40001` (serialization failure) and `40P01` (deadlock) are retried. That's at most **3 attempts** in total, waiting **10 ms** before attempt 2 and **50 ms** before attempt 3 (`engine.RetryBackoff`). Each attempt is a fresh transaction bounded by `transaction_timeout = 30s`, and one call including its retries is bounded by a 30 s budget. Business IDs (permit, reservation, operation, order, obligation, event) are generated before the first attempt and reused on every retry. When attempts are exhausted the call returns 503 `RETRIES_EXHAUSTED` (retryable) carrying the `operation_id` only if that operation already exists. Every other database error is classified, not retried: connectivity and timeouts → 503 `SERVICE_UNAVAILABLE`; anything else → 500 `INTERNAL_ERROR`. The backoff values are pinned defaults; production tuning would need load measurements, which R0 does not claim. Tests: `TestRetrySucceedsWithSameIdentities`, `TestRetriesExhaustedKeepOperationID`, `TestRetriesExhaustedBeforeReservationHasNoOperationID`, `TestRetryBackoffSpecification`.

**Proposed answers to the I00 questions (for review):**
- **Q1:** revocation releases unused reservations in the same transaction. Shown by the prototype.
- **Q2:** committed results remain readable after revocation.
- **Q3 (as implemented, D-6):** the cleanup scan selects candidates without locks and does **not** use `SKIP LOCKED`. Each candidate is then locked in the global order and rechecked, which is what guarantees one serial outcome. The cost is that cleanup may wait behind an admission holding the same mandate.
- **Q4:** the database clock is authoritative, plus an app-vs-DB skew check at readiness (`CLOCK_UNHEALTHY`).
- **Q5 (as implemented, D-7):** a signature that can no longer be published returns the reason the issuance was invalidated: `AUTHORITY_REVOKED` after revocation or an epoch change, `PERMIT_EXPIRED` after expiry, `REVISION_STALE` after an amendment. Tests: `TestLateSignatureAfterRevocation`, `TestT10LateSignatureAfterAmendment`.
- **Q6:** a recovered issuance keeps its original `expires_at`.
- **Q7:** amendments are keyed by `(tenant, instruction, issuer_principal, amendment_id)`.
- **Execution deadline (D-8):** the permit's `expires_at` is the execution deadline. It is the permit lifetime (at most 5 minutes) clamped to the mandate's expiry, so it can never exceed it. There is no separate deadline field.

**Open question:** should issuers be able to pin `expected_supplier_revision` at creation? Currently the server binds the live supplier revision.

## 9. Locking prototype (PROTOTYPE, not product)
`prototypes/locking`: minimal SQL stand-ins for the I03–I06 operations, with no signing and no HTTP.

**How each race runs.** Transaction A takes its locks and pauses at a barrier. Transaction B starts. The test waits until `pg_stat_activity` reports B with `wait_event_type = 'Lock'`, then lets A commit. No sleeps decide the ordering.

| Scenario | Outcome shown |
|---|---|
| Revocation first / admission first | Admission rejected `AUTHORITY_REVOKED` and the reservation released / one order committed; revocation increments the epoch without undoing it |
| Cleanup first / admission first | Admission rejected `PERMIT_NOT_ACTIVE` / one order, cleanup skips |
| Expiry after lock wait | `now()` is still before expiry but `clock_timestamp()` is after it, so the rejection is `PERMIT_EXPIRED` |
| Amendment first / admission first | Admission rejected `REVISION_STALE` and the reservation released once / amendment rejected `ALREADY_COMMITTED` |
| Late signature after amendment / signature first | Signature discarded and never published / amendment still invalidates and releases once |
| Two concurrent INR 6,00,000 reservations from 10,00,000 | One reservation; INR 4,00,000 available |
| Constraint backstops; role privileges | Direct invariant breaks are rejected by the database; executor DELETE, supplier edits and every observer write are denied |

Every scenario asserts the full persisted state and the bucket conservation identity. The suite was run 10 times in a row with no failures. In `ci/required.json` these map to T01, T07, T08, T10, T22, T25 and T28 as `complete: false`, so the report shows them as partial evidence and the cases stay NOT_RUN.

## 10. Commands
```bash
scripts/test-unit          # contract, keys, server, OpenAPI, CI gate
scripts/test-integration   # harness and locking prototype (needs STEELMATTER_TEST_ADMIN_URL)
scripts/test-conformance   # golden vectors, packet negatives, schema copy, cross-runtime signatures
scripts/test-verifier      # TypeScript verifier against the same vectors
scripts/ci                 # all of the above plus the aggregate gate (stage I02)
go run ./cmd/smvectors     # regenerate vectors (reviewed change only)
```
