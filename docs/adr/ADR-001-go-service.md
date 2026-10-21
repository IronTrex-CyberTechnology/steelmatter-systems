# ADR-001: Go service instead of Spring Boot

Status: **Accepted**. Reviewed and adopted for dependent implementation.
Date: 2026-10-12
Baseline: 0.6 (packet revision 0.6.2)

## Context
Document 02 specifies "one Spring Boot service with logical modules and one PostgreSQL instance". Documents 03, 04 and ADR-000 §12 require that Java and TypeScript produce identical canonical bytes, digests and signing inputs. The team has decided to implement the R0 service in Go.

## Decision
1. The R0 service is one Go binary (`cmd/steelmatter`) with logical packages under `core/`. It still uses one PostgreSQL instance and separate executor/observer database roles.
2. Wherever the contract says "Java" for the engine runtime, read "Go". The TypeScript verifier is unchanged. T21 and T41 cross-language checks become **Go ↔ TypeScript**: identical canonical payload bytes, digests and JWS signing inputs for fixed fixtures; each runtime verifies the other's fresh signatures; fresh signature byte equality is not required.
3. Transaction semantics are unchanged: READ COMMITTED, `SELECT … FOR UPDATE`, database constraints and the global lock order from document 02. No ORM and no optimistic-locking substitute. Post-lock time checks use `clock_timestamp()`, not `now()`, because `now()` is fixed at transaction start.
4. Pinned toolchain (see `versions.env`):

| Component | Version | Notes |
|---|---|---|
| Go | 1.27.3 (`go.mod` `toolchain go1.27.3`) | Upgraded from 1.27.1 on 2026-10-21 for the standard-library fixes govulncheck reported (GO-2026-6603, -6605, -6607, -6608, -6610, -6611, -6612, -6613, -6617, -6809) |
| PostgreSQL | 17.10, image `postgres:17.10@sha256:7958605b…3317` | Matches the local Homebrew server used for development |
| Node | 26.11.0 | TypeScript verifier (I07) |
| pgx | v5.11.0 | PostgreSQL driver and pool; no ORM |
| GitHub Actions | checkout v7.0.1, setup-go v7.0.0, upload-artifact v7.0.2 | Pinned to full commit SHAs in `.github/workflows/ci.yml` |

5. Libraries pinned in I02 (see [I02-contract.md §1](../contracts/I02-contract.md)):
   - Go: `go-jose/v4` v4.1.5, `gowebpki/jcs` v1.0.2, `santhosh-tekuri/jsonschema/v6` v6.0.3.
   - TypeScript: `jose` 6.2.12, `canonicalize` 5.1.0, `ajv` 8.20.0, `ajv-formats` 3.0.1.

   No custom cryptography. The strict JSON parser is our own because neither standard library rejects duplicate keys or keeps integers exact; it is not cryptography.

## Consequences
- **Strict JSON input is our own responsibility.** `encoding/json` silently accepts duplicate object keys and decodes numbers into floats. I02 must provide a strict decoder that rejects duplicate keys and unknown fields and keeps integers exact up to 2^53−1 (T20, T41).
- Go's `crypto/ecdsa` signatures are randomised, which is compatible with the contract's "fresh signatures need not match" rule.
- The real-PostgreSQL harness uses an admin URL (`STEELMATTER_TEST_ADMIN_URL`) rather than Testcontainers. This avoids a large dependency tree and the need for a Docker socket. CI uses a pinned service container; local runs use any PostgreSQL 17.10 server or `scripts/dev-postgres`.
- Documents 02, 03, 04 and the I02 checklist still say Java/Spring Boot. The packet is not edited (its manifest is hash-checked); this ADR is the record of the substitution.

## Review
- Implementation reviewer: Uday
- Second reviewer: Kaif
