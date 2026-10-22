# Steelmatter Systems R0 source preview

This preview demonstrates exact-action control for a synthetic purchase-order workflow. It is not a production deployment or a universal agent-security guarantee.

## Included behaviour

- Owner-issued mandates, shared INR allowances, instruction versioning and exact-action permits.
- Admission checks, reservation accounting and duplicate-order prevention against the simulated destination.
- Signed permit, execution and observation records, with offline verification in Go/TypeScript-compatible formats.
- A TypeScript SDK, a stdio MCP adapter with service-enforced authorisation, CLI scenarios and recorded browser playback.
- Access rules recorded in ADR-002: tenant isolation, scoped owners/actors, current workflow entitlement for issuers, and tenant-wide observer/operator reads.

## Verified source baseline

The source baseline tested by the release-check workflow is `515fa1f52c828aa7837a15e42c2192860394cbae` (21 October 2026):

| Check | Observed result |
|---|---|
| Automated I08 gate | 340/340 required checks passed |
| Fresh runner to a protected synthetic action with offline-verified evidence | 75 seconds; target 900 seconds |
| Full CI suite elapsed time | 120 seconds |
| Go vulnerability scan | govulncheck 1.9.0, database updated 21 October 2026: no vulnerabilities found |
| Log secret-pattern scan | No matches in 15 log/result files |
| Source artifact | 232 files; matched the baseline Git tree on retrieval |

These figures describe that baseline and runner, not every installation or the final public artifact. Subsequent closure changes concern documentation, export packaging and the reviewed full-R0 gate configuration. Product code, test assertions and dependency pins are unchanged. The release record supplies final-commit workflow links and the exact source/archive checksums.

Independent QA reports 214 execution passes and one review closure on that baseline. Separately, the 35 R0 catalogue assertions and test implementations were checked against 297 passing raw-result references. This mapping check does not replace independent QA. The active `full-R0` profile now enforces those reviewed cases plus all 340 I08 checks across seven phases, including demo. Nine R1/R2 cases remain out of scope; this is not a claim that they passed.

## Limits and distribution

- One organisation, INR integer paise and synthetic records only.
- A simulated purchase-order destination; no live banking, procurement or payment integration.
- The observer uses a separate read-only role but shares host/database administrators with the executor.
- The injection scenario uses a deterministic stand-in agent. Browser screens replay a recorded run.
- npm packages remain private. This release is a source-only preview with the Apache-2.0 licence and notices.
- Only files explicitly listed in `ci/public-export-files.txt` enter the source bundle. Source Git history is not part of that bundle.

Follow [docs/quickstart.md](docs/quickstart.md) for installation and the first verified action.

## Accepted CI tooling limits

The standalone aggregator can accept stale result files and can retain the last terminal result when repeated runs are concatenated. These are accepted limitations for this R0 source preview, not claimed fixes. Use the supported fresh-checkout `scripts/ci`/release workflow, which clears raw result files and runs each phase once. Do not combine or reuse old raw results as release evidence. Verify the workflow commit and archive manifest before relying on a release result.
