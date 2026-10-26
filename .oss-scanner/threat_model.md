# Threat model

Steelmatter Systems R0 is a source-only developer preview: a Go service that decides whether an autonomous agent may place one exact purchase order, enforces a shared allowance, prevents duplicate orders and exports signed evidence that a standalone TypeScript verifier checks offline. It runs locally with synthetic data and a simulated destination. R0 is the reference for later releases, so design and code flaws matter even though it is not deployed. See `SECURITY.md` for our disclosure policy and scope; this file follows it.

## What this project does and where untrusted input enters
- **HTTP API** (`core/server`, `core/httpapi`, `core/engine`, `core/auth`; spec in `api/openapi.yaml`). Every request body, header, path parameter and bearer token is untrusted. Callers are authenticated principals (owner, issuer, actor, observer, operator), each in one tenant. Treat any principal as potentially malicious towards other tenants, other principals and the allowance.
- **Contract parsing** (`core/contract`): strict JSON, RFC 8785 canonicalisation, payload validation and SM-JWS-ES256-1 signatures. Mandates, actions, permits, execution and observation records are attacker-supplied until verified. The rules are in `docs/contracts/I02-contract.md` and `schemas/`.
- **Evidence verifier** (`packages/verifier`, CLI at `packages/verifier/src/cli.ts`): evidence bundles are fully untrusted. The trust store passed with `--trust` is trusted.
- **SDK and MCP adapter** (`packages/sdk`, `adapters/mcp`): the agent driving the MCP adapter is untrusted and may be prompt-injected. It must not act outside its assigned actor, whatever the tool arguments say.
- **Trusted**: the database administrator, the host, the signing keys in `.local/keys`, environment variables and the operator running the service.

## Components that matter most / least
Most important (security invariants; see `docs/engine.md`, `docs/evidence.md` and the ADRs in `docs/adr/`):
1. Exact-action admission: a permit authorises only the exact action digest it was issued for. Any way to execute a different, altered or replayed action is critical.
2. Allowance and duplicate prevention: overspending the shared allowance, or placing the same order twice, including through races, retries, crashes or lost responses (`core/engine`, `core/db`).
3. Signatures and evidence: forging, altering or replaying permits, execution or observation records so that they verify; the verifier reporting broken or incomplete evidence as complete or consistent (ADR-003).
4. Authorisation: read or write access beyond ADR-002, especially cross-tenant access. Unauthorised readers must get a 404 identical to a missing ID.
5. Authentication and token handling, and executor/observer database role separation (`core/auth`, `core/db`).
6. Parser differentials between Go and TypeScript (canonicalisation, JSON strictness, JWS) that let one side accept what the other rejects.

Lower priority but in scope: `cmd/steelmatter` CLI subcommands, the SDK's retry logic.

Out of scope:
- `demo/` (recorded playback), `prototypes/`, `examples/`, the simulated destination and the deterministic stand-in agent.
- `cmd/smci`, `tools/ciagg` (CI aggregation; its stale-result limitations are documented in `RELEASE_NOTES.md`), `cmd/smvectors`, `scripts/`.
- The synthetic keys, tokens and fixtures committed under `fixtures/` and tests.
- The documented limitations in `RELEASE_NOTES.md`, including executor and observer sharing a database administrator.

## How to exercise it
- PostgreSQL 17.10 is installed in the image but not running. Start it with `sm-pg-start`. `STEELMATTER_TEST_ADMIN_URL` and `STEELMATTER_ADMIN_URL` already point at it (127.0.0.1:5432).
- `scripts/ci` runs every phase. Individual phases: `scripts/test-unit`, `scripts/test-integration` (needs PostgreSQL), `scripts/test-conformance`, `scripts/test-verifier`, `scripts/test-sdk`, `scripts/test-mcp`. Results go to `.out/*.json`.
- `tests/integration/` is the best harness for new reproducers: each test creates and drops its own database and roles (`core/dbtest`). Build tags: `integration,failpoints`. `core/failpoint` can inject crashes at named points.
- `DEMO_SCENARIO=flagship scripts/demo` runs the service end to end. Binaries from `cmd/` are prebuilt in `/usr/local/bin`. Synthetic keys are in `.local/keys`.
- Offline verification: `node packages/verifier/src/cli.ts verify --trust T --bundle B`.
- Golden vectors: `fixtures/vectors/golden-v1.json`.

## How you rate severity
- **Critical**: executing an action without a valid permit, executing an action other than the permitted one, spending beyond the allowance, duplicate order placement, forging any signed record that verifies, or cross-tenant write access.
- **High**: cross-tenant or ADR-002-violating read access to operation status or evidence; authentication bypass; the verifier accepting tampered, reordered, truncated or replayed evidence; the MCP adapter or SDK letting an agent act as another actor; a Go/TypeScript parsing differential that changes an admission or verification result.
- **Medium**: a 404/403 response difference that discloses whether another tenant's operation exists; rate-limit bypass; leaking token hashes or other secrets in errors or logs; a reachable vulnerable dependency without a demonstrated exploit path.
- **Low / informational**: hardening suggestions, defence-in-depth, error-message wording.
- Not reportable: denial of service by resource exhaustion against a local instance; anything that needs a compromised host, database administrator or trusted signing key, unless the result is evidence that the verifier wrongly accepts.

## How we would like reports
- Name the component, the commit, and the invariant or acceptance case (T-number from `ci/acceptance-cases.json`) that is broken, if known.
- Give a reproducer as a Go test in `tests/integration/` or a verifier test, using synthetic fixtures only.
- Keep patches minimal. Never weaken a check to make a test pass, and keep `scripts/ci` green.

## Anything to leave alone
- Constant-looking passwords in CI configuration and in this image's PostgreSQL setup are disposable, loopback-only test values.
- Missing TLS on the local listener, and the simulated destination accepting anything, are by design in R0.
