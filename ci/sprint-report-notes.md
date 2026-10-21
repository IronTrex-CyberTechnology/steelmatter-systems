## Scope and interpretation

The generated tables report the commit, environment, mapped tests and outcomes of this run. Automated test results and reviewed acceptance-case completion are separate: a case remains NOT_RUN until its mapping is reviewed and marked complete. The active full-R0 stage enforces the 35 reviewed R0 mappings and all 340 retained I08 checks across seven phases. Review was completed by Codex under the project owner's instruction; independent QA is attributed separately in the release notes.

## Reproduction

Run `scripts/ci` from the repository root. Database-backed tests require a disposable PostgreSQL 17.10 cluster and `STEELMATTER_TEST_ADMIN_URL` with CREATEDB and CREATEROLE. Toolchain versions are pinned in `versions.env`. Follow `docs/quickstart.md` to run a protected synthetic action and verify its evidence offline.

## Evaluation limits

The reference uses one organisation, INR integer paise and a simulated purchase-order destination. Its observer is a separate read-only database role, not an independent attestation authority. The injection scenario uses a deterministic stand-in agent. The browser demonstration is recorded playback with browser-side verification of evidence.

See `RELEASE_NOTES.md` for the tested source baseline and source-preview scope. The full-R0 profile contains 35 cases; the sprint-day4 report covers 31 of them.

Standalone aggregation does not establish fresh-run provenance and may retain the last terminal result from concatenated runs. For release evidence use a fresh checkout and `scripts/ci`, then match its commit to the audited source archive.
