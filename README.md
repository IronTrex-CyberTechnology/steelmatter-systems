# IronTrex Steelmatter Systems

**Open-source execution authority for AI agents.**

Steelmatter controls which purchase orders an agent can place in Odoo 18. A mandate sets the spending limit. A permit binds the purchase to a specific supplier, delivery address, line items and total. When tenant policy requires human approval, the engine checks the latest decision as it commits the order to dispatch.

| Event | What happens to the order and the allowance |
|---|---|
| The approver withdraws before dispatch commits | Dispatch returns HTTP 403. Odoo receives no order. The existing reservation follows its expiry and release rules. |
| Dispatch commits before withdrawal | Withdrawal returns HTTP 409. The admitted operation keeps its reference and can be settled or reconciled. |
| Odoo creates the order and its reply is lost | The amount stays held. The engine looks up the original operation reference and checks Odoo’s order before settling the amount. |
| A restore makes the engine and its recovery anchor disagree | Steelmatter blocks new writes. An operator reconciles the records and resumes writes after the recovery checks pass. |

Earlier R1 revisions accepted dispatch after approval withdrawal. The correction and its Odoo tests are documented in the [release notes](RELEASE_NOTES.md) and [validation record](docs/validation.md).

## Run Steelmatter

| Evaluation path | What you can verify |
|---|---|
| [Local quickstart](docs/quickstart.md) | Run a purchase against the R0 synthetic destination and verify its signed evidence offline. |
| [Odoo 18 purchasing](docs/odoo-purchasing-coverage.md) | Inventory purchasing access on a clone. Use a separate enforcement installation to test the protected agent route and ordinary buyer access. |
| [Recovery procedures](docs/r1-recovery-operations.md) | Inspect an unresolved operation, reconcile it against destination records and check the conditions for resuming writes. |

The R1 qualification covers isolated infrastructure, synthetic data, one tenant, one Odoo company, one active backend and INR amounts in integer paise. Database owners and privileged administrators are trusted. Odoo.sh enforcement and production workloads require separate qualification. [Deployment and trust boundaries](docs/r1-decisions.md)

## Interfaces

- **HTTP API and TypeScript SDK.** Issue mandates, evaluate purchases, dispatch approved orders and inspect outcomes. [OpenAPI contract](api/openapi.yaml) · [SDK](packages/sdk/README.md)
- **MCP adapter.** An assigned agent can evaluate an action, admit an R0 synthetic operation and retrieve evidence over stdio. R1 Odoo purchases use the HTTP API or SDK. [MCP tools](adapters/mcp/README.md)
- **Evidence verifier.** Check signed execution and observation records offline. The browser demo replays recorded R0 scenarios and runs the verifier locally. [Evidence format](docs/evidence.md) · [Browser demo](demo/README.md)
- **Go engine.** PostgreSQL transactions enforce shared allowances and exact-action admission. A separate recovery anchor records dispatch and destination events across restores. [Engine](docs/engine.md) · [Contracts](docs/contracts/I02-contract.md)

## Local verification

The pinned toolchain is Go 1.27.3, PostgreSQL 17.10 and Node 26.11.0. Use a disposable PostgreSQL instance with a login that can create databases and roles. With Docker, `scripts/dev-postgres` prints the connection setting. [Version pins](versions.env)

```bash
export STEELMATTER_ADMIN_URL='postgres://USER@localhost:5432/postgres?sslmode=disable'
scripts/bootstrap
(cd packages/verifier && npm ci --ignore-scripts)
DEMO_SCENARIO=flagship scripts/demo
export STEELMATTER_TEST_ADMIN_URL="$STEELMATTER_ADMIN_URL"
scripts/ci
```

`scripts/ci` runs seven phases and checks their results against the [required tests](ci/required.json) and [acceptance cases](ci/acceptance-cases.json). The separate Odoo suite counts orders in Odoo’s database and checks the engine’s ledger. The [validation record](docs/validation.md) links the measured results to the source tested and gives the commands for the Odoo gate.

## Licence

The source distribution includes the [Apache License 2.0](LICENSE), [NOTICE](NOTICE) and [third-party notices](THIRD_PARTY_NOTICES.md). The TypeScript packages build from source in this repository.
