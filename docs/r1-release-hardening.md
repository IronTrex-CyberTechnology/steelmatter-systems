# R1 release hardening

Baseline: private `uday-r1` at `af7d1a3b07eaeb2704a0678fc7bdda85f87caf8c`.
This change is a review candidate, not a release or customer-write approval.

## Recovery and repair changes

- A real connector requires the engine recovery anchor. Readiness fails while writes are fenced.
- Startup disables writes and runs the recovery gate before HTTP admission or workers start. A failed gate leaves recovery endpoints available with writes off.
- Evaluation and admission compare the mandate's engine records with its anchor history while holding the budget row lock. This check has no age grace period. Missing history disables writes before more money can be reserved or admitted.
- A failed background restore comparison disables writes. An unreadable store is not a successful comparison.
- Human repair decisions are recorded durably before engine updates. The two databases do not share a transaction. A retry or restore reconciliation applies the same saved decision once, including when writes are fenced. Reviews and confirmations serialize on the mandate and dispatch locks.
- Recovery comparisons inside repair confirmation reuse the transaction connection. Concurrent confirmations cannot exhaust the pool while waiting for the mandate lock.

## R1 evidence contract

- Execution and observation payloads accept `schema_version: "2"` for external destinations. R0 version 1 remains unchanged; mandates, actions and permits remain version 1. Existing signature types and digest namespaces remain unchanged; the signed payload's version distinguishes its semantics.
- A version 2 execution includes `admitted_at` and `company_ref`. Admission must be within the signed permit and mandate windows; confirmation may arrive later, but cannot precede admission. Observation cannot precede confirmation. A version 1 observation cannot attest a version 2 execution.
- Dispatch records preserve admission time in the engine and recovery anchor. Settlement stores immutable destination facts. The evidence worker builds and signs a separate R1 execution, without inserting a synthetic R0 order or moving budget again.
- The destination order result now includes `supplier_revision`, captured when Odoo creates the order. Missing or differing revisions cannot count as a verified matching purchase. Update the engine, connector and Odoo module together.
- The observer uses its separate database and Odoo read-only credentials. Its signed version 2 record includes expected/observed company and permit digest, and observed order state. Matching commercial fields alone do not prove a match in another company or a cancelled order. Readback never releases money or recreates orders.
- The worker refreshes previously observed R1 orders after one minute (100 oldest due orders per pass), with per-operation failure backoff. Missing execution records and overdue readbacks appear in backlog alerts. The real Odoo test edits, cancels and deletes a confirmed order and checks exported evidence and unchanged commitment after each step.
- Existing evidence/status/reconcile endpoints expose the R1 records. The Go validator and standalone TypeScript verifier support both versions. Tests exercise signed Go exports through the TypeScript CLI, including admission before expiry followed by confirmation after expiry and engine restore.
- Migration 0014 adds separate R1 evidence tables and read views. Existing unrecovered dispatches use their original creation time as admission time. A restored legacy record with no original admission timestamp, or a manual accounting repair without verified destination facts, remains explicitly incomplete. No historical timestamps or purchase evidence are invented.

## Verification

The active aggregate CI stage is `r1-engine`. Real Odoo recovery tests and a build of the exact export allowlist are additional steps in the required `steelmatter-ci` job. Skipped or missing real-Odoo results fail that step.

Developer checks and independent SDET verification remain separate. Sindhu should receive the final fixed commit, test results, contract changes and setup instructions after development checks complete.

## Release decisions (settled 29 Oct 2026)

- T17–T19 (exact-action human approval) stay in R1 and are implemented; see [r1-decisions.md](r1-decisions.md), decision 9.
- HELD money is released only with anchor proof that no purchase occurred and that the reference is permanently blocked; a three-person repair alone cannot release it (decision 3). Automated recovery never uses a human note as proof of no purchase.
- This is a proposed version 2 contract addition. Both developers must include it in their shared interface review before merging; existing external verifiers must be upgraded to verify version 2 records.
