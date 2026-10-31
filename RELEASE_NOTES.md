# Steelmatter Systems R1 preview candidate

R1 adds external Odoo purchase-order admission and outcome reconciliation to the R0 synthetic procurement reference. This source preview is for evaluation with synthetic data and isolated infrastructure. Publication does not approve production purchases.

## Included behaviour

- Owner-issued mandates, shared INR allowances, versioned instructions and exact-action permits.
- Odoo 18 integration with protected supplier, delivery, quantity, unit and tax details.
- Retry-safe operation references and held budget for uncertain outcomes; settlement after destination verification.
- Recovery anchoring, restore detection, reconciliation and gated resumption of writes.
- Human approval separation, tenant isolation, restricted runtime database credentials and one active backend.
- Signed execution and observation evidence with offline verification, a typed HTTP SDK and recorded browser playback.

The R0 quickstart remains a synthetic workflow. The browser demo replays pinned R0 scenarios; it is not a live demonstration of Odoo purchases. MCP purchase creation is outside the R1 rollout scope.

## Verification boundary

The functional baseline independently retested by QA is `9514149aa13e86a20f07e3d57b12947859448a6e`. Demo and packaging changes are based on `69b8c34377096a63e9fc3fe8ae7a9c5749934b03`. The current candidate includes documentation corrections after that commit. Use the final release commit, its workflow results and its source manifest as the publication evidence; earlier counts do not certify a later artifact.

`scripts/ci` runs the active `r1-engine` gate: 527 required test identities across seven phases. Real-Odoo integration is a separate gate in the CI workflow. Required test identities, executed assertions and QA acceptance cases must not be described as interchangeable totals.

The release bundle includes a `COMMIT` file, `MANIFEST.sha256`, `SHA256SUMS` and `AUDIT.txt`. The demo reports the source commit and identifies the recording commit separately. Only the reviewed files in `ci/public-export-files.txt` enter the source bundle.

## Supported evaluation scope and accepted limits

- One tenant, one Odoo company, one active backend; INR integer paise.
- Start deployment evaluation in observe-only mode. Enabling external purchases requires an explicit rollout decision and the documented configuration.
- Database owners and privileged administrators remain trusted. Runtime role restrictions do not claim protection from a compromised database administrator.
- Observer-only database outages can delay observation after reconnection. QA reproduced approximately 80 seconds before eventual MATCH in one test; this is an accepted operational delay, not a fixed defect or a guaranteed maximum.
- Recovery from historical pre-fix anchored revision conflicts is not covered by the fresh-deployment qualification. Read the recovery procedures before restoring existing data.
- Source-only Apache-2.0 preview; npm packages remain private.

The old R0-only descriptions and accepted aggregator limitations are superseded for this candidate. Current aggregator regression checks cover stale, duplicate, missing, skipped and failed results. Do not reuse or concatenate old phase outputs as release evidence.

Follow [the quickstart](docs/quickstart.md), [R1 decisions](docs/r1-decisions.md), and [recovery operations](docs/r1-recovery-operations.md). QA-owner approval and release-owner approval remain separate records tied to the final candidate.
