# IronTrex Steelmatter Systems

Steelmatter is an IronTrex CyberTechnology project for bounded machine-action authority, exact-action admission and verifiable evidence. R1 extends the R0 synthetic procurement reference to an Odoo 18 sandbox, with recovery anchoring, outcome reconciliation and protected purchase details. This is a developer preview for evaluation, not a production deployment approval.

The local quickstart uses the R0 synthetic destination. R1 checks external purchase orders, preserves allowance reservations while outcomes are uncertain, and reconciles outcomes before settlement. Scope is one tenant, one Odoo company, one active backend and INR integer paise.

The service is written in Go ([ADR-001](docs/adr/ADR-001-go-service.md)). Start with the synthetic [quickstart](docs/quickstart.md), then follow its R1 sandbox section. Read the [R1 decisions and limits](docs/r1-decisions.md) and [recovery procedures](docs/r1-recovery-operations.md) before evaluating external execution.

## Status
- **Contracts:** [docs/contracts/I02-contract.md](docs/contracts/I02-contract.md) covers strict JSON input, payload validation, RFC 8785 digests, SM-JWS-ES256-1 signing, Go/TypeScript golden vectors, the closed OpenAPI spec and the PostgreSQL locking prototype.
- **I03–I06 core transactions:** implemented and developer-tested; see [docs/engine.md](docs/engine.md). 40 integration scenarios cover T01–T16, T20, T22, T23, T27, T28 and T39–T41, including real process-crash tests.
- **I07 evidence:** read-only observer, signed execution and observation records, evidence export and reconcile, and the standalone TypeScript verifier and CLI; see [docs/evidence.md](docs/evidence.md).
- **I08 demo:** `scripts/demo` runs the flagship, stale-supplier, lost-response and agent-injection scenarios from a clean state; see [docs/quickstart.md](docs/quickstart.md). `scripts/ci` also writes the 31-case day-4 report to `.out/sprint-day4-report.md`.
- **SDK:** `packages/sdk` is a typed client for the R0 and R1 HTTP APIs, with timeouts and contract-aware retries; see [packages/sdk/README.md](packages/sdk/README.md). 
- **MCP adapter:** `adapters/mcp` is a thin stdio MCP server forwarding authenticated calls through the SDK; the service enforces assigned-actor execution and ADR-002 read access; see [adapters/mcp/README.md](adapters/mcp/README.md).
- **Browser demo:** implemented as recorded playback with browser-side evidence verification; see [demo/README.md](demo/README.md).
- **Acceptance reporting:** the active `r1-engine` gate is defined in [ci/required.json](ci/required.json). It requires 527 test identities across seven phases. The separate real-Odoo gate runs external execution and recovery scenarios. Required identities, executed assertions and independent QA cases are different counts; report them separately.
- **Distribution:** source preview only. npm packages remain `private: true`. See [RELEASE_NOTES.md](RELEASE_NOTES.md) for measured checks and limitations.

## Requirements
Pinned in [versions.env](versions.env): Go 1.27.3, PostgreSQL 17.10, Node 26.11.0. Library pins are in `go.mod` and `packages/verifier/package-lock.json`.

## Quick start
Full walkthrough: [docs/quickstart.md](docs/quickstart.md).
```bash
export STEELMATTER_ADMIN_URL='postgres://USER@localhost:5432/postgres?sslmode=disable'
scripts/bootstrap                       # verify tools, modules; generate synthetic keys
(cd packages/verifier && npm ci --ignore-scripts)
DEMO_SCENARIO=flagship scripts/demo      # protected synthetic action and offline evidence verification
export STEELMATTER_TEST_ADMIN_URL="$STEELMATTER_ADMIN_URL"
scripts/ci                              # all test phases, aggregate gate and acceptance report
```
No local PostgreSQL? With Docker running, `scripts/dev-postgres` starts the pinned image and prints the URL to export.

The admin URL needs `CREATEDB` and `CREATEROLE`. Each integration test creates its own database (`sm_test_<random>`) plus executor and observer login roles, then drops them. Set `STEELMATTER_TEST_KEEP_DB=1` to keep the database for inspection.

## Commands
| Command | Purpose |
|---|---|
| `scripts/bootstrap` | Check pinned tools, verify modules, generate synthetic keys; with `STEELMATTER_ADMIN_URL`, also create `steelmatter_dev` and write `.local/dev.env` |
| `scripts/test-unit` | Unit tests (no DB) → `.out/unit.json` |
| `scripts/test-integration` | Real-PostgreSQL tests → `.out/integration.json` |
| `scripts/test-conformance` | Golden vectors, packet negatives and Go↔TypeScript fresh signatures → `.out/conformance.json` |
| `scripts/test-verifier` | TypeScript verifier against the same vectors → `.out/verifier.json` |
| `scripts/test-mcp` | Thin MCP adapter: typecheck, build, protocol, tool and stdio tests → `.out/mcp.json` (the live agent test runs in the integration phase) |
| `scripts/test-sdk` | TypeScript SDK: strict typecheck, unit, contract and installed-package tests → `.out/sdk.json` (live SDK tests run in the integration phase) |
| `go run ./cmd/smvectors` | Regenerate `fixtures/vectors/golden-v1.json` (a reviewed contract change) |
| `scripts/ci` | All phases required by the active stage, then the aggregate gate → `.out/results.json` |
| `node packages/verifier/src/cli.ts verify --trust T --bundle B` | Verify an evidence bundle offline (see [docs/evidence.md](docs/evidence.md)) |
| `scripts/demo` | One-command demonstration from a clean state |
| `go run ./cmd/smdemo setup\|run\|reset` | Demo client against a running service |
| `go run ./cmd/smci report` | Stage checkpoint report (default `sprint-day4`) |
| `scripts/package` | Build `steelmatter` and `smkeys` into `dist/` with SHA-256 sums |
| `scripts/env-check` | Probe the isolated runner for D04–D06 |
| `go run ./cmd/steelmatter serve` | Run the API and workers (see [docs/engine.md](docs/engine.md) for configuration) |
| `go run ./cmd/steelmatter principal-add -id ID -kind KIND` | Create a synthetic principal and print its token once (`STEELMATTER_DB_ADMIN_URL`) |

## CI gate
`steelmatter-ci` is the single required check. [`ci/required.json`](ci/required.json) defines the stages:
- **I01**: foundation.
- **I02**: contracts.
- **I03-I06**: core transactions.
- **I07**: evidence.
- **I08**: the 340-check implementation baseline.
- **sprint-day4**: the 31-case checkpoint.
- **full-R0**: the retained R0 acceptance stage.
- **r1-engine**: the active stage, including R1 execution, recovery and demo provenance checks.

The gate fails when a required phase is missing, any test fails, is skipped or doesn't finish, a required test is absent, or a required case is not PASS. Every one of the 44 catalogue cases appears in the report. A case is PASS only when its mapping is marked complete and every mapped test passed.

## Layout
```
cmd/                 steelmatter (service), smkeys (synthetic keys), smci (CI gate), smvectors (golden vectors)
core/                engine packages: contract (parser, payloads, canonical, JWS), db (roles, migrations),
                     dbtest (harness), keys (trust store), httpapi (error catalogue), server
api/                 openapi.yaml (closed OpenAPI 3.1) and its consistency tests
schemas/             the five payload JSON Schemas (packet, unchanged)
packages/verifier/   standalone TypeScript verifier (@irontrex/steelmatter-verifier, private)
packages/sdk/        typed TypeScript API client (@irontrex/steelmatter-sdk, private); see its README
adapters/mcp/        thin MCP adapter for an actor agent over stdio (@irontrex/steelmatter-mcp, private)
fixtures/            packet examples and golden vectors
prototypes/locking/  I02 locking prototype (PROTOTYPE, not product)
tests/conformance/   cross-language conformance suite
tools/ciagg          aggregate gate logic
ci/                  required results per stage, acceptance catalogue
scripts/             entry points
docs/                public technical docs, ADRs and contracts
```

## Licence
This source preview is distributed under the unmodified [Apache License 2.0](LICENSE), [NOTICE](NOTICE) and [third-party notices](THIRD_PARTY_NOTICES.md). Names and marks are governed by Apache-2.0 section 6.
