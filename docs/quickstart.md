# Quickstart

Run the Steelmatter R0 procurement reference locally with synthetic data. No production service, credential or model API is needed.

## Requirements
- Go 1.27.3 and PostgreSQL 17.10 (pinned in `versions.env`).
- Node 26.11.0 for offline evidence verification and the TypeScript packages.
- No local PostgreSQL? With Docker running, `scripts/dev-postgres` starts the pinned image and prints the URL to export.

## Run the demonstration
```bash
export STEELMATTER_ADMIN_URL='postgres://USER@localhost:5432/postgres?sslmode=disable'
scripts/bootstrap
(cd packages/verifier && npm ci --ignore-scripts)
scripts/demo
```
`scripts/demo` runs everything from a clean state:
- builds release binaries (no test hooks) and generates synthetic ES256 keys;
- creates a fresh database with separate executor and read-only observer roles, then starts the service;
- runs the four scenarios below;
- verifies the flagship evidence offline with the standalone verifier, if Node dependencies are installed;
- drops the database. Set `DEMO_KEEP=1` to keep it.

It prints the elapsed time and writes `.out/demo.json`.

| Scenario | Shows |
|---|---|
| `flagship` | INR 10,00,000 allowance; two agents request INR 6,00,000 at once. One reservation, INR 4,00,000 available. Commit, then retry returns the same order with no second charge. Observer MATCH and signed evidence. |
| `stale-supplier` | The supplier record changes after the permit is issued: rejected until the issuer amends and the agent re-evaluates. |
| `lost-response` | The order commits but the response is dropped in transit. Lookup shows the commit; retry returns the original order. |
| `agent-injection` | Supplier text tells the agent to switch supplier, inflate the order and raise its allowance. Every attempt is rejected; only the exact permitted order commits. A deterministic stand-in agent is used; no model is called. |

Run one scenario with `DEMO_SCENARIO=flagship scripts/demo`.

Each scenario prints, and `.out/demo.json` records, the commit, the tests that cover it and the SHA-256 of every fixture it uses. The run is compared with the pinned results in `fixtures/demo/reference-results-v1.json` (T43); a changed outcome or fixture fails the demo until the reference is regenerated and reviewed:
```bash
STEELMATTER_UPDATE_DEMO_REFERENCE=1 go test -tags integration,failpoints -run '^TestT43DemoEvidenceConsistency$' ./tests/integration
```

Accounting continuity (T29) uses the labelled synthetic invoice/payment-reference fixture in `fixtures/accounting/`. It is linked to the committed obligation as administrator fixture data and never changes the allowance; no bank is called.

## Against a long-running service
```bash
scripts/bootstrap                                      # keys in .local/keys, modules verified
STEELMATTER_ADMIN_URL=... go run ./cmd/steelmatter db-init   # creates steelmatter_dev, writes .local/dev.env
set -a; source .local/dev.env; set +a
STEELMATTER_KEYS_DIR=.local/keys go run ./cmd/steelmatter serve &
go run ./cmd/smdemo setup -tokens .local/demo/tokens.json
go run ./cmd/smdemo run -server http://127.0.0.1:8080 -tokens .local/demo/tokens.json -out .local/demo/out
go run ./cmd/smdemo reset                              # back to the synthetic starting state
```

## Verify evidence yourself
```bash
(cd packages/verifier && npm ci --ignore-scripts)
node packages/verifier/src/cli.ts verify --trust .local/keys/trust-store.json --bundle .local/demo/out/flagship-evidence.json
```
The report lists integrity, issuer, profile, usability, linkage and observation separately. There is no overall "safe" verdict.

## Tests and the day-4 report
```bash
export STEELMATTER_TEST_ADMIN_URL="$STEELMATTER_ADMIN_URL"
scripts/ci     # all phases, the aggregate gate, then .out/sprint-day4-report.md and .out/sprint-day4-results.json
```

## Limitations
Read [engine.md](engine.md) and [evidence.md](evidence.md). In short:
- one organisation, INR only, synthetic destination;
- the observer is a separated readback, not independent attestation.

## Browser demo
```bash
(cd packages/verifier && npm ci --ignore-scripts)
cd demo && npm ci --ignore-scripts && npm run build   # then open demo/dist/index.html
```
Recorded playback of the pinned demo run, labelled as such, and evidence checked in the browser by the standalone verifier. See [demo/README.md](../demo/README.md).
