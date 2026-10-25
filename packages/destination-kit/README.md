# Steelmatter Destination Adapter Kit

Apache-2.0 **experimental authoring and conformance kit**, version 0.1.0-preview.1.
Source preview; not published to npm. No runtime dependencies, build step,
credentials, model calls or network access. Node 24.22.1+; the parent repository
uses its pinned Node version in CI.

The kit helps adapter authors describe and test how an exact **non-payment**
action reaches a destination and how its outcome can be checked. It is **not**
an authorization service, a live connector, or a production security boundary.
It is not wired into the R0 Go engine. See [SECURITY.md](SECURITY.md) before use.

## Try it

From the repository root, with Node installed:

```bash
node --test packages/destination-kit/test/*.test.mjs
node packages/destination-kit/examples/demo.mjs
node packages/destination-kit/src/cli.mjs scaffold my-destination
```

The demo uses only an in-memory membership record. It deliberately loses the
execution response, reconciles **without another write**, and compares readback
with the requested change. It prints `APPLIED`, `MATCH`, and one destination
write, with explicit synthetic/unsigned/no-authorization labels.

`scaffold` prints a JSON file map containing `manifest.json` and `adapter.mjs`.
Review and save the contents yourself. All four generated methods throw
`ADAPTER_NOT_IMPLEMENTED`; every capability starts as `unsupported`. The CLI
does not write files, import generated code, fetch API documentation or call a
destination. This version is a deterministic scaffold generator, **not** an
AI/OpenAPI-to-connector generator.

To inspect a saved manifest:

```bash
node packages/destination-kit/src/cli.mjs inspect path/to/manifest.json
```

Exit codes: `0` = capabilities declared, **not verified**; `1` = unsupported
capabilities block the declared profile; `2` = invalid input or usage. The
report always says `implementation_verified: false` and
`authorization_granted: false`. Passing schema validation never permits a write.

## Included / deliberately excluded

| Included in this preview | Not implemented by this kit |
|---|---|
| Closed manifest validation and fail-closed scaffolding | Reviewing or certifying generated connector code |
| Immutable, digest-bound action snapshot | Signed permits, issuer trust, current entitlement or revocation checks |
| Target, tenant, actor, version, operation and parameter binding | Shared authority accounting, delegation or aggregate budgets |
| Atomic check/change/journal **in one synthetic process** | Distributed atomicity, crash-safe storage, real API conditional writes |
| Readback comparison and eight fixture conformance scenarios | Independent third-party outcome attestation or signed evidence |
| Conservative `UNKNOWN` handling in the demo | Durable capacity holds, automatic retries or production recovery workers |

The existing [R0 engine](../../docs/engine.md) and
[evidence verifier](../../docs/evidence.md) have separate contracts. No R0
payload, API, acceptance mapping or signed-evidence format is changed here.
An adapter binding cannot be submitted as an R0 permit. Money movement remains
outside this kit's scope; DigiAuth is the separate boundary.

## Manifest contract

The [synthetic manifest](examples/memory-adapter.mjs) is a complete example.
Required root fields are `schema`, `adapter_id`, `adapter_version`,
`environment`, `operations`, and `limitations`. Every operation declares:

| Field | Allowed values / meaning |
|---|---|
| `name` | Unique operation identifier |
| `effect` | `non-payment` only; this declaration cannot detect a dishonest implementation |
| `parameters` | 1–32 named fields, each `string`, `integer` or `boolean`; all required, no extras |
| `precondition` | `atomic-version-check` or `unsupported` |
| `idempotency` | `atomic-operation-journal` or `unsupported` |
| `reconciliation` | `operation-journal-read` or `unsupported` |
| `observation` | `separate-read-path` or `unsupported` |

Unknown fields, unknown enums and duplicate operation names are rejected.
`parseManifest(Uint8Array)` additionally rejects duplicate JSON keys, invalid
UTF-8/BOM, lone surrogate escapes, fractional/exponent/unsafe integers,
negative zero, trailing content, depth over 32 and input over 64 KiB. The
strict parser is adapted from this repository's verifier, with a smaller limit.
Direct object APIs reject accessors, custom prototypes, non-JSON values,
symbols, cycles and sparse arrays. Treat object inputs as local data, not
untrusted JavaScript; use byte parsing at an external boundary.

## Action binding and adapter interface

```js
import { bindAction, compareObservation } from './src/index.mjs';

const action = bindAction(manifest, {
  operation_id: 'operation-1', tenant_id: 'tenant-a', actor_id: 'actor-a',
  destination_id: 'directory-a', target_id: 'member-1',
  operation: 'member.set-enabled', expected_version: '1',
  parameters: { enabled: false },
});
```

All request fields and the manifest digest participate in `action_digest`.
Changing any material field, actor, tenant, destination or manifest requires a
new binding **and new authority admission**. Anyone can compute a new digest:
a hash is not approval and does not check live authority. The kit's sorted-key,
integer-only encoding and domain-separated SHA-256 profile are experimental;
they are **not** the core's RFC 8785 / SM-JWS profile. Pin both adapter code and
manifest independently; a manifest hash does not attest the implementation.

The adapter contract uses the same bound action for four separate calls:

| Method | Result and obligation |
|---|---|
| `precheck(action)` | `{status: READY/STALE/UNKNOWN}`. Advisory only; cannot eliminate a check/use race. |
| `execute(action)` | Atomically recheck the approved version, change the destination and record operation identity. Never silently substitute parameters or targets. |
| `observe(action)` | Read actual material state through a separate read path; `null` means unavailable/insufficient. Do not echo requested values or reuse the write response. |
| `reconcile(action)` | Read operation history without mutating the destination. An absent record, timeout, or unavailable read stays `UNKNOWN`, not `NOT_APPLIED`. |

`execute` / `reconcile` return one of:

```js
{ status: 'APPLIED', operation_id, action_digest, target_version }
{ status: 'NOT_APPLIED', operation_id, action_digest, reason: 'STALE_VERSION' }
{ status: 'UNKNOWN', operation_id, action_digest }
```

`NOT_APPLIED` is valid only when the destination definitively rejected the
change and no attempt can still commit. A timeout is not such proof. A new
version must differ from `expected_version`; production adapters must define
non-reused version tokens and durable journal retention covering their entire
retry/recovery window. The in-memory fixture satisfies neither durability nor
restart recovery and cannot be used to demonstrate them.

Observation shape:

```js
{ operation_id, action_digest, tenant_id, destination_id, target_id,
  target_version, values: { enabled: false } }
```

`compareObservation(manifest, action, execution, observation)` returns `MATCH`,
`MISMATCH`, or `INSUFFICIENT_EVIDENCE`. It compares exact material values and
identity/version linkage. It cannot authenticate the observer, verify causal
provenance, prove freshness, or attest that a separate principal performed the
read. The sample uses the same process for all functions. `MATCH` is a data
comparison result, **not** cryptographic evidence or approval to release capacity.

## Conformance fixtures

```js
import { runConformance } from './src/conformance.mjs';
import { createFixture } from './examples/memory-adapter.mjs';
const report = await runConformance(createFixture);
if (!report.passed) throw new Error('fixture conformance failed');
```

The factory must return a fresh, disposable synthetic environment containing
`manifest`, `request`, `adapter`, and synchronous test hooks `mutate()`,
`loseNextResponse()`, `setReadUnavailable(boolean)`, `writeCount()`. Hooks must
actually mutate the test destination, drop an applied operation's response,
disable read access, and count execution writes. Tests assume a versioned
material record and the error markers shown in the reference fixture.

Eight cases cover apply/readback, identical replay, operation-ID conflict,
change-after-precheck, lost response/reconciliation, unresolved uncertainty,
readback drift, and concurrent identical calls. Additional unit tests cover
scope substitution, binding, parser inputs and two competing operations.
The harness refuses an `external` manifest, but a dishonest declaration can
still misdirect test code. Never supply production credentials or endpoints.
The report is scenario evidence, not certification; it always sets
`certification: false`.

## Before a real integration

Keep the trusted authority and destination executor outside the agent's control.
Review destination API semantics and bypass paths. Establish genuine atomic
conditional writes and durable operation lookup before claiming support; if
the API cannot provide them, declare `unsupported`. Integrate live authorization
checks and reservation ownership with the core through a separately reviewed
contract. Keep capacity held on uncertain outcomes, reconcile before any
retry, and verify actual state with restricted observer credentials. Add
crash/restart, multi-process, revocation, stale-read, retention and bypass tests.
This preview implements none of those production integration steps.

Contributions should include capability limitations and reproducible negative
tests. Keep unsupported guarantees explicit. Source is covered by the root
[Apache-2.0 license](LICENSE) and [NOTICE](NOTICE).
