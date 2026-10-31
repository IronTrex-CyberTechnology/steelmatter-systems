# Quickstart

Run the synthetic R0 procurement reference first, then evaluate the R1 Odoo sandbox separately. Neither walkthrough needs a production service, production credential or model API.

## Requirements
- Go 1.27.3 and PostgreSQL 17.10 (pinned in `versions.env`).
- Node 26.11.0 for offline evidence verification and the TypeScript packages.
- No local PostgreSQL? With Docker running, `scripts/dev-postgres` starts the pinned image and prints the URL to export.

Run all commands from the repository root. Use a disposable PostgreSQL cluster: the admin account needs `CREATEDB` and `CREATEROLE`.

For the Docker option, run `scripts/dev-postgres`, copy the printed `export STEELMATTER_TEST_ADMIN_URL=...` command, and then set:
```bash
export STEELMATTER_ADMIN_URL="$STEELMATTER_TEST_ADMIN_URL"
```
The demo/bootstrap uses `STEELMATTER_ADMIN_URL`; integration tests use `STEELMATTER_TEST_ADMIN_URL`.

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

It prints the elapsed time and writes `.out/demo.json`. Evidence and trust-store files are retained under the run-specific `.local/demo_<timestamp>_<pid>/` directory printed at startup. `.out/demo.json` records this live run; the browser demo below is recorded playback.

| Scenario | Shows |
|---|---|
| `flagship` | INR 10,00,000 allowance; two agents request INR 6,00,000 at once. One reservation, INR 4,00,000 available. Commit, then retry returns the same order with no second charge. Observer MATCH and signed evidence. |
| `stale-supplier` | The supplier record changes after the permit is issued: rejected until the issuer amends and the agent re-evaluates. |
| `lost-response` | The order commits but the response is dropped in transit. Lookup shows the commit; retry returns the original order. |
| `agent-injection` | Supplier text tells the agent to switch supplier, inflate the order and raise its allowance. Every attempt is rejected; only the exact permitted order commits. A deterministic stand-in agent is used; no model is called. |

Run one scenario with `DEMO_SCENARIO=flagship scripts/demo`.

Each scenario prints, and `.out/demo.json` records, the commit (from git, or in a release bundle from its `COMMIT` file), the tests that cover it and the SHA-256 of every fixture it uses. The run is compared with the pinned results in `fixtures/demo/reference-results-v1.json` (T43); a changed outcome or fixture fails the demo until the reference is regenerated and reviewed:
```bash
STEELMATTER_UPDATE_DEMO_REFERENCE=1 go test -tags integration,failpoints -run '^TestT43DemoEvidenceConsistency$' ./tests/integration
```

Accounting continuity (T29) uses the labelled synthetic invoice/payment-reference fixture in `fixtures/accounting/`. It is linked to the committed obligation as administrator fixture data and never changes the allowance; no bank is called.

## Against a long-running service
```bash
export STEELMATTER_ADMIN_URL='postgres://USER@localhost:5432/postgres?sslmode=disable'
scripts/bootstrap                                      # also creates steelmatter_dev and .local/dev.env
set -a; source .local/dev.env; set +a
STEELMATTER_KEYS_DIR=.local/keys go run ./cmd/steelmatter serve &
go run ./cmd/smdemo setup -tokens .local/demo/tokens.json
go run ./cmd/smdemo run -server http://127.0.0.1:8080 -tokens .local/demo/tokens.json -out .local/demo/out
go run ./cmd/smdemo reset                              # back to the synthetic starting state
```

## Verify evidence yourself
```bash
(cd packages/verifier && npm ci --ignore-scripts)
node packages/verifier/src/cli.ts verify --trust .local/keys/trust-store.json --bundle .local/demo/out/flagship-evidence.json --fail-on integrity,issuer,profile,linkage,chronology,observation
```
These paths belong to the long-running-service walkthrough. For `scripts/demo`, use `keys/trust-store.json` and `out/flagship-evidence.json` inside its printed run directory.

The report lists integrity, issuer, profile, usability, linkage and observation separately. There is no overall "safe" verdict.

## Tests and the day-4 report
```bash
export STEELMATTER_TEST_ADMIN_URL="$STEELMATTER_ADMIN_URL"
scripts/ci     # all phases, the aggregate gate, then .out/sprint-day4-report.md and .out/sprint-day4-results.json
```

## R1 Odoo sandbox

R1 uses the Odoo 18 sandbox and a separate recovery-anchor database. Docker with Compose, Go, Node and Python 3 are required. Keep the engine test cluster from the Docker option above running and export `STEELMATTER_TEST_ADMIN_URL` before continuing.

```bash
destinations/odoo/sandbox/up
destinations/odoo/sandbox/test-engine -json > .out/odoo.json
python3 tools/check-odoo-results.py .out/odoo.json
```

Run the engine suite against a fresh Odoo sandbox. Before repeating it, `destinations/odoo/sandbox/reset` deletes that sandbox's local data; run it only when you intend to discard those test records, then run `up` again. The engine and Odoo share recovery history, so reusing a previous run is not a clean test.

For connector-only checks, start a fresh sandbox and run `destinations/odoo/sandbox/test-connector -v`. Review [R1 configuration and limits](r1-decisions.md) and [recovery operations](r1-recovery-operations.md). These are evaluation steps, not production rollout instructions.

## Limitations
Read [engine.md](engine.md), [evidence.md](evidence.md) and [R1 decisions](r1-decisions.md):
- one tenant/company, one active backend and INR only;
- the R0 walkthrough is synthetic; R1 integration uses the Odoo sandbox;
- the observer is a separated readback, not independent attestation;
- trusted database administrators remain inside the trust boundary;
- observer reconnection can delay readback after an outage; eventual matching is not an immediate-observation guarantee;
- MCP purchase creation is outside the R1 rollout scope. Begin deployment evaluation in observe-only mode.

## Browser demo
```bash
(cd packages/verifier && npm ci --ignore-scripts)
cd demo && npm ci --ignore-scripts && npm run build   # then open demo/dist/index.html
```
Recorded playback of the pinned demo run, labelled as such, and evidence checked in the browser by the standalone verifier. See [demo/README.md](../demo/README.md).
