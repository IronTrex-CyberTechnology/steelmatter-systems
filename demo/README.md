# Steelmatter browser demo

A small scenario demonstrator for the R0 reference (document 13). Simulation
with synthetic data only. No login, real ERP, payment or approval button.

## Build and open
```bash
cd packages/verifier && npm ci --ignore-scripts && cd ../..   # the verifier's pinned dependencies
cd demo && npm ci --ignore-scripts && npm run build
```
Open `demo/dist/index.html` in a browser. No server and no network are needed.

## What it shows
- **Scenarios (screen A, B):** recorded playback of the pinned demo run,
  `fixtures/demo/reference-results-v1.json` (T43). Every screen is labelled
  *Simulation*, *Synthetic data* and *Recorded playback, not a live run*, and
  shows the recorded commit, case, test IDs and fixtures. The recording stores
  step names, pass/fail and the end ledger; the explanation of each step comes
  from the scenario source and lives in `src/notes.ts`.
- **Evidence (screen C):** the synthetic evidence chains in
  `fixtures/vectors/evidence-v1.json`, checked in the browser by the standalone
  verifier. Integrity, issuer, execution state, observation and linkage are
  shown separately, plus usability inside the permit window and now.
- **Developer quickstart (screen D).**

## How the verifier runs in the browser
`build.mjs` bundles `packages/verifier/src` unchanged with esbuild. Its Node
calls are served by small stand-ins in `src/shims`:
- `node:crypto` sha256 through `@noble/hashes` (no custom cryptography;
  signatures use jose with WebCrypto);
- `node:fs` schema reads from the same schema files, bundled;
- `node:module` for Ajv, from the verifier's own pinned dependencies;
- `Buffer` base64url and UTF-8 only.

`test/verifier.test.ts` runs every golden vector and evidence chain through
the browser bundle and the Node verifier and requires identical results.

## Tests (CI phase `demo`)
```bash
scripts/test-demo
```
Strict typecheck, static build with no network references, browser-verifier
parity, and the T43 page checks: each scenario screen shows exactly what the
recording contains, and a recording change without a matching note fails.

## Limitations
- The T11 card has no recording yet: no demo scenario exists for it in
  `examples/procurement/scenarios.go`.
- The recording has no per-step amounts or reason codes; the page shows the
  start and recorded end ledger, and reason codes from the scenario source.
- The evidence screen uses the verifier fixture chains, not evidence from the
  recorded scenario runs.
