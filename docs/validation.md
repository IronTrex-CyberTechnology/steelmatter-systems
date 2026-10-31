# Validation evidence and measurement scope

Steelmatter R1 implements bounded execution authority for the supported Odoo 18 purchase-order path. Its validation covers authorisation, budget conservation, destination execution, recovery and signed evidence. This page separates those exercised behaviours from deployment qualification and performance measurements.

## Verified source

- Source commit: [`f8b6ebe26c845c4990959b7abbeec8ae58345dd9`](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/commit/f8b6ebe26c845c4990959b7abbeec8ae58345dd9).
- Source tree: `12a78b209d3318cb49f84b56e8c1a0a5f232a0b0`.
- Required workflow: [`steelmatter-ci`, 31 October 2026](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/actions/runs/41196339479).
- Environment: GitHub-hosted Ubuntu 24.04; Go 1.27.3; Node 26.11.0; PostgreSQL 17.10; isolated Odoo 18 sandbox. Dependency and container pins are in the workflow, lockfiles and [versions.env](../versions.env).

These results are a dated snapshot of that exact source. Later documentation or code changes need their own CI result; a green historical run does not validate a different artifact.

## Test results

| Phase | Passed test records | Failed | Skipped |
|---|---:|---:|---:|
| Unit | 218 | 0 | 0 |
| Integration | 491 | 0 | 0 |
| Conformance | 160 | 0 | 0 |
| TypeScript verifier | 231 | 0 | 0 |
| TypeScript SDK | 48 | 0 | 0 |
| MCP adapter | 20 | 0 | 0 |
| Browser demo | 26 | 0 | 0 |
| **Seven-phase total** | **1,194** | **0** | **0** |
| Separate real-Odoo execution and recovery gate | 22 | 0 | 0 |

The aggregate gate required **527 test identities** across the seven phases. Its acceptance report recorded **41 PASS, zero FAIL, zero BLOCKED and three NOT_RUN**. The three excluded cases are T36 (external risk signal outage), T37 (delegation narrowing) and T38 (multiple customer tenants); each belongs to R2. The source-export build and Go tests also passed.

Executed records include subtests. Required identities are the gate's selected tests. Acceptance cases group those tests by behaviour. These counts measure different things and must not be added together or presented as independent security audits.

The workflow retains sanitised phase JSON and the aggregate report as its `steelmatter-ci-results-f8b6ebe26c845c4990959b7abbeec8ae58345dd9` artifact. Reproduce the seven phases with `scripts/ci`; reproduce the separate destination gate with `destinations/odoo/sandbox/up` and `destinations/odoo/sandbox/test-engine -json`, followed by `python3 tools/check-odoo-results.py .out/odoo.json`. Follow the [quickstart](quickstart.md) for database setup and prerequisites.

## Demonstrated execution behaviour

| Property | Exercised behaviour and source |
|---|---|
| Shared budget under contention | [T02 fault-free test](../tests/integration/capacity_test.go): 100 concurrent distinct instructions request 10,000,000 paise each against a 100,000,000-paise allowance. Exactly ten permits and 90 capacity denials; persisted reservations and ledger conservation are checked. [The separate failure-injection test](../tests/integration/races_test.go) checks that at most ten permits issue and capacity remains conserved. |
| Exact purchase authorisation | Action tampering, actor/audience substitution, stale supplier/delivery revisions and unit/tax changes are rejected or recorded as mismatches. See [R1 decisions](r1-decisions.md), decisions 1, 2 and 9. |
| Retry and crash safety | Replay, lost responses and real process-crash tests check duplicate prevention in the supported paths. An uncertain external outcome keeps its exposure held until authoritative reconciliation. See [the acceptance catalogue](../ci/acceptance-cases.json), T09 and T13–T16/T33. |
| Restore recovery | New writes stay fenced while engine and recovery-anchor state disagree. Recovery resumes across bounded calls and preserves held amounts. See [recovery procedures](r1-recovery-operations.md) and the separate real-Odoo suite. |
| Verifiable evidence | Go and TypeScript conformance, signed execution/observation linkage, tamper rejection and offline verification are exercised. See [evidence](evidence.md). Observer readback uses separate credentials within the documented shared-administrator trust boundary. |

These are properties checked in specific scenarios. Passing the suite does not establish absence of defects, universal duplicate prevention across arbitrary destinations, or a service availability guarantee.

## Performance measurements

The repository includes a [sandbox anchor/Odoo timing script](../destinations/odoo/tests/anchor_timing.py) for p50/p95/p99 latency and eight-writer anchor append throughput. No result from that script is asserted by the required CI run above, and no complete evaluate-to-verified-Odoo-outcome throughput or latency benchmark is published here.

[Recovery timeout measurements](r1-recovery-timeout.md) document a specific experiment: a stub destination, real PostgreSQL, a fixed 20 ms delay per anchor query, and 24 operations recovered after an engine restore. The documented observations are approximately 150 ms for a three-operation restore check and approximately 90 ms per recovered operation. They are scoped recovery measurements, not production purchase latency, maximum recovery delay or a comparison against another product. The operations-per-call example on that page is an estimate using stated network assumptions.

To publish a deployment performance claim, retain the source commit, hardware and topology, dataset size, configuration, workload mix, concurrency, warm-up and run duration, repeated runs, error/timeout counts, p50/p95/p99 latency and resource use. Measure authorisation, dispatch, destination confirmation, observation and recovery separately. A throughput test must also check conserved budget and duplicate effects at the destination. Installation time and CI duration describe developer workflow, not transaction performance.

## Deployment qualification

R1's documented scope is one tenant, one Odoo company, one active backend and INR integer paise. Use synthetic data and isolated infrastructure for evaluation, starting in observe-only mode. MCP purchase creation is excluded. Multiple active backends and multi-customer-tenant operation are outside this qualification.

The [documented security work](r1-decisions.md) includes secret management and credential rotation, separate API/worker database roles, database/anchor mTLS, anchor-write alerting and HSM/KMS signing-key integration. Database owners and privileged administrators remain trusted. The application defaults to a loopback HTTP listener; any external deployment must define its authenticated network boundary and transport security.

There is no production load/soak/SLO qualification or independent penetration-test evidence in the verification record above. Deployment-specific threat review, access/key lifecycle, transport controls, monitoring, backup/restore exercises and sustained workload qualification remain necessary before asserting production readiness for a stated environment.

An observer database outage can delay readback after reconnection. QA observed about 80 seconds before eventual MATCH in one case; this is neither a latency target nor a maximum-delay guarantee. Historical pre-fix anchored-revision conflicts remain outside fresh-deployment qualification. See [release notes](../RELEASE_NOTES.md) for accepted limits.
