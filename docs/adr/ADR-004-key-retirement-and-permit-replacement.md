# ADR-004: Key retirement is one-way once effective; retried permits from retired keys are replaced

Status: **Proposed**, 2026-10-27. Needs two non-author approvals (see Review below).
Date: 2026-10-27
Baseline: 0.6 (packet revision 0.6.2)
Closes security findings SEC-DEF-001 (retry returns a permit signed by a retired key) and SEC-DEF-004 (a second retirement reactivates a retired key).

## Context
A trust-store key may carry `not_after`, retiring it from that instant (I02 §5, historical trust keys). A retired key can no longer sign, and a permit it signed no longer authorises admission (403 `JWS_KEY_UNTRUSTED`, "re-evaluate for a new permit"). Two gaps were reported against this control.

**SEC-DEF-001.** `Evaluate` returns the existing issuance unchanged on an identical retry if it is `ACTIVE`, unexpired and in the current control epoch (`core/engine/evaluate.go`). It did not check the key that signed `permit_jws`. After a rotation, the up-front signer check passes (a current key exists), so re-evaluating an instruction with a live permit returned the old permit. Admission refused it and told the caller to re-evaluate, which returned the same permit again. The operation was blocked, and its capacity stayed reserved, for up to the permit lifetime (at most 5 minutes) after every rotation. It was not an authorisation bypass, and the ledger stayed conserved.

**SEC-DEF-004.** `TrustStore.Retire` (and `smkeys retire`) overwrote `not_after` with any time after `created_at`. Retiring a key again with a later time silently reactivated a key that was already retired. A reactivated key could sign a new permit that admission accepted, and records signed in the reopened window appeared as valid evidence. Exploiting it needs write access to the trust store (a privileged operator or insider).

## Decision
1. **A retry replaces a permit that admission would refuse.** On an identical retry, the live `ACTIVE`, unexpired, current-epoch issuance is returned only if its permit still verifies under the trust store and its key is not retired now. That is exactly the rule `Admit` applies (`permitKeyCurrent`). Otherwise, under the same locks as the retry:
   - the issuance is released with the new `release_reason = 'KEY_RETIRED'` and its capacity returned;
   - a new permit (new permit, reservation and operation IDs) is issued and reserved as for a first evaluation, then signed with the current key;
   - the response is **201** with the new permit, because a new permit was issued.

   Release and re-reservation happen in one transaction, so capacity is never double-reserved and the ledger identity holds throughout. A later identical retry returns the replacement unchanged (200). Migration `0009_release_reason_key_retired.sql` adds `KEY_RETIRED` to the `issuances.release_reason` check.

2. **Status of the superseded operation.** `GET /v1/operations/{id}` for the superseded operation ID reports authorisation `EXPIRED`, like other unused, released issuances. The API enum is unchanged, so OpenAPI, SDK and MCP adapter need no change; the OpenAPI `OperationStatus` description names the case.

3. **An effective retirement can only move earlier.** `TrustStore.RetireAt(name, at, now)` refuses to set `not_after` later once the current `not_after` has taken effect (`not_after <= now`), returning `ErrRetirementEffective` without changing anything. Moving it earlier, or to the same time, is allowed, for example when a compromise is found to have started earlier. `Retire(name, at)` keeps its signature and uses the current time.

4. **A retirement not yet in effect may be rescheduled either way.** An operator who scheduled a future retirement can move it later or earlier until it takes effect. Once it has taken effect, the only way to sign again is to provision a new key. `smkeys retire` prints the old and new `not_after`, and on refusal names the time the key was retired and leaves the trust store file unchanged. There is no override flag.

## Alternatives considered
- **Return a definite refusal instead of a new permit (SEC-DEF-001).** Simpler, and needs no new release reason. Rejected because the caller still cannot proceed until the old permit expires, and its capacity stays reserved for that time. The admission message ("re-evaluate for a new permit") would also stay untrue.
- **Never allow `not_after` to move later, even before it takes effect (SEC-DEF-004).** Strictest, and needs no ADR. Rejected because operators schedule retirements ahead of a rotation and need to postpone them if the rotation slips. A retirement that has not taken effect has not yet removed any authority, so postponing it reactivates nothing.
- **An override flag on `smkeys retire`.** Rejected: it is exactly the path a compromised key would be reactivated by.

## Consequences
- After a rotation, an instruction with a live permit from the retired key gets a usable permit on its next evaluation instead of being blocked for up to 5 minutes.
- An issuance can now end as `RELEASED`/`KEY_RETIRED`. Anything that reads `release_reason` must accept the new value. Inside this repository, the status derivation (`core/engine/workers.go`) maps it to `EXPIRED`.
- Each evaluation retry of a live permit now verifies the stored permit signature (one ES256 verification) inside the transaction.
- A rescheduled future retirement is accepted without any record of the earlier value in the trust store; `smkeys retire` prints both values so the operator's log has them.
- Moving an effective retirement earlier can make records signed in the removed window untrusted (their evidence becomes INVALID). That is the intended effect after a compromise, and is unchanged by this ADR.

## Tests
- `TestSecDef001RetryAfterKeyRotationIssuesCurrentPermit` (integration): rotate the permit key, re-evaluate, check the old issuance is `RELEASED`/`KEY_RETIRED`, the new permit is signed by the current key, the ledger is reserved once, an identical retry returns the replacement, it is admitted, and the superseded operation reports `EXPIRED`.
- `TestRetireCannotReactivateKey` (`core/keys`) and `TestRetireRefusesReactivation` (`cmd/smkeys`): rescheduling before effect, refusal after effect (including exactly at `not_after`), moving earlier, and an unchanged trust store file on refusal.

Each test was checked to fail with its fix removed.

## Review
- Author: Kaif
- Reviewer 1: _pending_
- Reviewer 2: _pending_
