# ADR-002: Authorised readers of operation status and evidence

Status: **Accepted**, 2026-10-21. Two non-author approvals recorded (see Review below).
Date: 2026-10-17
Baseline: 0.6 (packet revision 0.6.2)
Raised by: QA clarification C10 (CPT-053)

## Context
Document 03 says that an "authorised reader" may call `GET /v1/operations/{id}`, `GET /v1/operations/{id}/evidence` and `GET /v1/adapters/{id}/assurance`, but does not define who that is. It does fix two things: 404 means "absent/inaccessible identity without disclosure", and T23 requires that a query with an unmapped or wrong-tenant identity discloses nothing sensitive.

The implementation (`core/engine/evidence.go`, `scope`) applies one rule. The repository documentation described it inconsistently: `docs/evidence.md` said "the instruction's actor or issuer", which reads as the issuer that created the instruction, while the code uses the issuer's current entitlement to the instruction's source workflow (review finding F-23).

This ADR records the rule as implemented so that QA, documentation and code agree. It changes no behaviour.

## Decision

### Operation status and evidence
`GET /v1/operations/{id}` and `GET /v1/operations/{id}/evidence` return 200 only to an authenticated principal **in the operation's tenant** that is one of:

| Principal kind | Condition |
|---|---|
| owner | It is the owner of the mandate the operation's permit was issued under. |
| issuer | It is **currently entitled** to the instruction's source workflow (a row in `issuer_entitlements` for the tenant, issuer and source system). This is the same rule that governs amendment; it is not "the issuer that created the instruction". |
| actor | It is the actor assigned to the instruction (`instructions.actor_id`). R0 never reassigns an instruction. |
| observer | Any observer of the tenant. |
| operator | Any operator of the tenant. |

Every other caller receives **404 `NOT_FOUND`** with the same response as for an operation ID that does not exist. This covers:
- a principal of another tenant (the tenant always comes from authentication, never from the request);
- an owner of a different mandate;
- an issuer not currently entitled to the source workflow;
- an actor other than the assigned one.

The response does not reveal whether the operation exists.

### Reconcile
`POST /v1/operations/{id}/reconcile` is limited to observer and operator principals of the operation's tenant. Everyone else gets 404 `NOT_FOUND`, as above.

### Adapter assurance
`GET /v1/adapters/{id}/assurance` describes the adapter, not an operation. Any actor, issuer, owner, observer or operator principal may read it. Any other principal kind gets 403 `FORBIDDEN`. An unknown adapter ID gets 404 `NOT_FOUND`.

## Consequences
- **Issuer access follows current entitlement.** If an issuer's entitlement to the source workflow is removed, that issuer can no longer read the operation, including its historical evidence. An issuer that becomes entitled can read operations of that workflow.
- **Observers and operators read tenant-wide, by design.** Their role is to watch and reconcile every operation of the tenant (reconcile is limited to them), so scoping them to individual mandates or instructions would defeat that role. They hold no write path to mandates, instructions, permits or admission (`x-steelmatter-principals` in `api/openapi.yaml`), so read access adds no authority. Their reach stops at the tenant boundary: another tenant's operations return 404 like any other out-of-scope read. Owners, issuers and actors read only operations within their own scope.
- **No existence oracle.** Callers without access cannot tell an existing operation from an absent one (F-23, T23).
- The allowed principal kinds per endpoint stay declared in `api/openapi.yaml` (`x-steelmatter-principals`). The scope conditions above are applied by `core/engine/evidence.go` (`scope`, `readers`) and `core/engine/simulate.go` (`Assurance`).

## Evidence
- Code: `core/engine/evidence.go` (`scope`, `readers`, `Reconcile`, `Evidence`), `core/engine/workers.go` (`GetOperation`), `core/engine/simulate.go` (`Assurance`).
- Tests (integration phase):
  - `TestT23CrossScopeRead`: owner-a, issuer-a, agent-a, observer-a and operator-a get 200; agent-b, issuer-b, owner-x, agent-x and issuer-x get 404.
  - `TestT23EvidenceAndReconcileScope`: the same rule for evidence; reconcile only for observer and operator.
  - `TestT23CrossTenantAdmitDisclosesNothing`.
- QA: CPT-053 observed exactly this behaviour (C10).
  - `TestADR002IssuerLosesReadAccessWhenEntitlementRemoved`: the creating issuer reads status and evidence while entitled, gets 404 (identical to an absent operation, no evidence records) once the entitlement is removed, other readers are unaffected, and access returns when the entitlement is restored.
  - `TestADR002NewlyEntitledIssuerGainsReadAccessWithoutCreatingInstruction`: an issuer that did not create the instruction gets 404 until it is entitled to the source workflow, then 200; an entitlement to a different workflow does not grant access.

## Review
- Author: Uday
- Kaif: approved, 2026-10-21 (non-author approval 1 of 2)
- Aakhil: approved, 2026-10-21 (non-author approval 2 of 2), recorded by Codex under his explicit instruction to complete the ADR-002 approval. The previously requested entitlement-transition tests and tenant-wide observer/operator rationale were reviewed before recording this approval.
- Verification: both `TestADR002*` tests and the three T23 scope tests passed in the release-check artifact for source commit `515fa1f52c828aa7837a15e42c2192860394cbae`. This accepts the access policy; it does not constitute patent clearance or final release acceptance.
