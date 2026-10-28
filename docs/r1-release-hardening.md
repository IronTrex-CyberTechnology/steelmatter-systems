# R1 release hardening

Baseline: private `uday-r1` at `af7d1a3b07eaeb2704a0678fc7bdda85f87caf8c`.
This change is a review candidate, not a release or customer-write approval.

## Recovery and repair changes

- A real connector requires the engine recovery anchor. Readiness fails while writes are fenced.
- Startup disables writes and runs the recovery gate before HTTP admission or workers start. A failed gate leaves recovery endpoints available with writes off.
- Evaluation and admission compare the mandate's engine records with its anchor history while holding the budget row lock. This check has no age grace period. Missing history disables writes before more money can be reserved or admitted.
- A failed background restore comparison disables writes. An unreadable store is not a successful comparison.
- Human repair decisions are recorded durably before engine updates. The two databases do not share a transaction. A retry or restore reconciliation applies the same saved decision once, including when writes are fenced. Reviews and confirmations serialize on the mandate and dispatch locks.

## Verification

The active aggregate CI stage is `r1-engine`. Real Odoo recovery tests and a build of the exact export allowlist are additional steps in the required `steelmatter-ci` job. Skipped or missing real-Odoo results fail that step.

Developer checks and independent SDET verification remain separate. Sindhu should receive the final fixed commit, test results, contract changes and setup instructions after development checks complete.

## Release decisions still open

- The full-R1 catalogue still includes T17–T19 (exact-action human approval). This patch does not silently remove those requirements or invent an approval design.
- The existing D5 human repair workflow permits a three-person reviewed release of HELD money. Its trust assumptions must be explicitly reconciled with the earlier FOUND/FENCED-only release rule before claiming full-R1 acceptance. Automated recovery never uses a human note as proof of no purchase.
- Signed R1 execution evidence and independent destination readback require a versioned contract that distinguishes admission time from later settlement. Reusing R0's commit-within-permit assertion would be incorrect.
