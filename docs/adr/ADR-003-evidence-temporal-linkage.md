# ADR-003: Cross-record timestamp checks in evidence linkage

Status: **Accepted**, 2026-10-21. Two non-author approvals recorded (see Review below).
Date: 2026-10-19
Baseline: 0.6 (packet revision 0.6.2)
Closes review finding F-11 ("verifier time linkage", deferred) and the QA report of six validly signed but temporally inconsistent evidence chains.

## Context
An evidence bundle carries four signed records: mandate (`issued_at`, `expires_at`), permit (`issued_at`, `expires_at`), execution (`committed_at`) and observation (`observed_at`). Until now:
- the TypeScript verifier and the Go service checked each record's own window (`expires_at` after `issued_at`, permit lifetime ≤ 5 minutes) and each record's time against its key's retirement;
- cross-record linkage compared digests, identifiers, tenant, actor/audience, scope and amounts, but no timestamps.

So a chain whose records are individually valid and correctly signed, but impossible in time (for example an execution committed after the permit expired, or an observation recorded before the execution it observes), was reported as `linkage: COMPLETE`, `observation: MATCH` by the verifier and as `AVAILABLE` by the service. This needs a trusted key to sign the wrong times (a signer bug, misconfiguration, compromise or backdating); an outsider cannot produce it. But a relying party verifying offline (T26; issue #5 federation) has only the bundle, and the verifier must not present such a chain as complete and consistent.

All signed timestamps come from one clock: PostgreSQL `clock_timestamp()` truncated to whole seconds (`core/engine/mandates.go`, `evaluate.go`, `admit.go`, `core/observer`). Admission checks permit and mandate expiry against that clock after taking its locks; evaluation clamps permit expiry to mandate expiry and denies when the mandate expires first; observation happens after commit. Every chain the service produces therefore satisfies strict ordering with no tolerance.

## Timestamp semantics (confirmed before the comparisons were written)
Confirmed from the code, the schemas and document 02:
- **Format:** every signed time is RFC 3339 UTC with whole seconds and a `Z` suffix; payload validation rejects anything else (I02 §3), in both runtimes.
- **Source:** every signed time is PostgreSQL `clock_timestamp()` truncated to the second: mandate `issued_at` (`core/engine/mandates.go`), permit `issued_at` (`evaluate.go`), execution `committed_at` (`admit.go`), observation `observed_at` (`core/observer`). Mandate `expires_at` comes from the owner's request; permit `expires_at` is `issued_at` plus the TTL, clamped to the mandate's expiry.
- **Expiry is exclusive** (document 02: "exclusive expiry and no positive grace"): admission requires `clock_timestamp() < expires_at` after its locks, and the consuming UPDATE repeats that guard.
- **Truncation:** a commit in the last fraction of the permit's final second is recorded as that second, which is still before `expires_at`; an observation in the same second as its commit records the same value. Equal values are therefore valid wherever the rule is "≤", and a commit recorded exactly at `expires_at` cannot come from the service.
- **Evaluation time is irrelevant** to these rules: they compare signed record times with each other, never with "now", so authentic historical evidence stays consistent after the permit expires (usability reports that separately).

## Decision
1. **Rules.** With timestamps as whole UTC seconds and expiry exclusive (document 02), a chain is temporally consistent only if all of these hold:

| Check name | Rule | Records |
|---|---|---|
| `time:permit_issued_within_mandate` | mandate.issued_at ≤ permit.issued_at < mandate.expires_at | mandate, permit |
| `time:permit_expiry_within_mandate` | permit.expires_at ≤ mandate.expires_at | mandate, permit |
| `time:execution_within_permit` | permit.issued_at ≤ execution.committed_at < permit.expires_at | permit, execution |
| `time:execution_within_mandate` | mandate.issued_at ≤ execution.committed_at < mandate.expires_at | mandate, execution |
| `time:observation_after_execution` | execution.committed_at ≤ observation.observed_at | execution, observation |

   Equal seconds are allowed where the rule says ≤, because timestamps are truncated to seconds. No clock-skew tolerance: one clock signs every record.

2. **Reporting and compatibility.**
   - **Verifier report version `"2"`.** A new top-level dimension, `chronology: { status: CONSISTENT | INCONSISTENT | INCOMPLETE, checks }`, reports exactly the five time checks, with a reason on each failed one. The same five checks are also part of `linkage`, so a version-1 consumer that gates on `linkage` still fails closed (`BROKEN`). No field was removed or renamed; consumers that pin `report_version: "1"` must update.
   - A violated rule is `MISMATCH`: chronology `INCONSISTENT`, linkage `BROKEN`. A rule whose records are absent is `MISSING`: chronology `INCOMPLETE`, linkage `INCOMPLETE`, as for a pending chain.
   - **CLI:** `--fail-on chronology` gates on chronology (exit 1 unless `CONSISTENT`); `--fail-on linkage` also catches every violation. The text report has a `chronology:` section naming each failed rule.
   - **Browser demo:** uses the same verifier source; its evidence screen shows a "Chronology" result and states "Evidence mismatch: impossible timeline". A parity test requires browser and Node reports to be identical on every case.
   - **Service:** a violating bundle is `evidence: INVALID` in `GET /v1/operations/{id}` and `/evidence` (Go `evidenceStatus`). The API response schema is unchanged (OpenAPI, SDK and MCP adapter need no change); a caller sees which rule failed by verifying the bundle.

3. **Parity.** `fixtures/vectors/evidence-temporal-v1.json` (generated by `go run ./cmd/smvectors -only temporal`, self-checked against the Go rules at generation) holds 13 validly signed chains that differ only in timestamps: the six reported scenarios (S1-S6, each labelled in `covers`), two further combinations and five valid controls (consistent; observation in the same second as execution; commit in the last second before permit expiry; every inclusive bound equal; a pending chain). Go (`TestConformanceTemporalEvidenceVectors`), TypeScript (`test/temporal.test.ts`, including the CLI gates and historical evaluation) and the browser bundle (`demo/test/verifier.test.ts`) must report exactly the expected broken checks and chronology status for every case. `evidence-v1.json` is not regenerated; its chains are consistent and keep verifying unchanged.

## Consequences
- Evidence that was previously reported as complete can now be reported as broken. Every chain the service produces stays consistent (all integration, evidence, demo and SDK tests still pass), and the existing signed fixtures still verify.
- If the database clock is stepped backwards (for example by an NTP correction) between two records of one operation, honest evidence would be reported as broken. The service already treats clock problems as fail-closed (`CLOCK_UNHEALTHY`); this is recorded as a known limitation rather than hidden by a tolerance.
- Revocation time is not in any signed record, so revocation ordering is still known only to the service (unchanged; the `usability` note already says so).
- Rules for other record kinds or future operation families (issue #1) need their own review.

## Review
- Author: Kaif
- Uday: approved, 2026-10-21 (non-author approval 1 of 2; implementation and CI verified on 434e47c)
- Aakhil: approved, 2026-10-21 (non-author approval 2 of 2)
