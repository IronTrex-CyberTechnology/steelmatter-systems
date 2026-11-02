# Steelmatter R1 for Odoo 18 purchasing

## Approval withdrawal at dispatch

Earlier R1 revisions could accept a purchase after an approver withdrew approval. A permit issued before withdrawal was sufficient to reach dispatch. This revision rechecks approval inside the transaction that commits dispatch.

| Which transaction commits first? | Result |
|---|---|
| Withdrawal | Dispatch returns HTTP 403. Odoo creates zero orders. An outstanding reservation follows its normal expiry and release rules. |
| Dispatch | A later withdrawal returns HTTP 409. Steelmatter tracks the admitted operation through settlement or recovery. In the tested successful purchase, Odoo recorded exactly one order. |

For an evaluation that relies on withdrawal, use the corrected engine and repeat both cases on the target installation before enabling the protected route. The [2 November Odoo CI run](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/actions/runs/41380213055) covers the correction on source `8240c95`. Its [clean-install and bundle checks](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/actions/runs/41380307758) cover the same revision. Each release archive identifies its own source commit and file checksums.

## Purchasing access

Install `steelmatter_coverage` on a clone to inventory purchasing routes, rights and known bypasses. It takes a configuration snapshot on request and installs no purchase-write hooks. On a separate enforcement installation, a designated manual buyer can create, edit and confirm ordinary orders. The guard prevents that buyer from changing a protected order or moving its lines. [Installation and coverage guide](docs/odoo-purchasing-coverage.md)

## Execution and recovery

R1 binds approval to the purchase terms, including supplier and delivery revisions, quantities, units and taxes. An operation reference links dispatch, Odoo’s purchase order and the ledger. If Odoo’s response is lost, the amount stays held until destination evidence resolves the outcome. Restore detection fences new writes while the engine reconciles against its independent recovery anchor. Operators can export signed execution and observation records for offline verification. [Recovery procedures](docs/r1-recovery-operations.md) · [Evidence](docs/evidence.md)

## Evaluation scope

- Use isolated infrastructure and synthetic data. Qualification covers one tenant, one Odoo company, one active backend and INR integer paise. Start with the coverage inventory or an observe-only engine. Enable writes only on a separately authorised test installation.
- Use the HTTP API or TypeScript SDK for R1 purchases. The local quickstart, recorded browser scenarios and MCP admission exercise the R0 synthetic destination.
- Database owners and privileged administrators remain trusted. Odoo.sh enforcement and production deployment require separate qualification.
- An observer database outage can delay readback. QA observed about 80 seconds from reconnection to `MATCH` in one test. The maximum delay is unmeasured.
- Fresh-install qualification excludes historical `REVISION_NUMBER_TAKEN` conflicts. Keep writes fenced and follow the [operator procedure](docs/r1-recovery-operations.md) for affected restores.

## Verification and distribution

Independent QA retested the functional baseline `9514149aa13e86a20f07e3d57b12947859448a6e`. The demo and packaging baseline is `69b8c34377096a63e9fc3fe8ae7a9c5749934b03`. The [validation record](docs/validation.md) distinguishes earlier results, the withdrawal regression and deployment-specific limits. Independent release approval applies to the candidate reviewed.

The source archive includes `COMMIT`. The release build also produces `MANIFEST.sha256`, `SHA256SUMS` and `AUDIT.txt`. The [export manifest](ci/public-export-files.txt) identifies the reviewed source files. Build the TypeScript packages from source. See the [R0 release](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/releases/tag/r0-source-preview.1) for the earlier synthetic workflow.
