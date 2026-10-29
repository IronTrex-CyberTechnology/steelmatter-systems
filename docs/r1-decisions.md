# R1 owner decisions of 29 Oct 2026: engine implementation

Aakhil's eleven R1 decisions, as implemented in the engine on top of `4f5f9a4`
(Kaif's D1 v1.4 module and connector). Decision 1 includes tax support and
replaces the earlier zero-tax instruction. Each row names the behaviour and the
tests that pin it. Odoo-side parts are Kaif's and are listed only for context.

| # | Decision | Engine behaviour now | Tests |
|---|---|---|---|
| 1 | Exact purchase details, with tax | An issuer may propose `unit_id` and `tax_ids` per line and one `tax_minor`. The action authorises the exact quantities, units and taxes, `amount_untaxed_minor` (the line total), `tax_minor` and `amount_minor` = untaxed + tax. The full total is held and verified on the Odoo order. Taxes without a tax amount, or a tax amount without taxes, are refused (400). Nothing is converted or dropped. Odoo refuses a product whose configured purchase taxes differ from the authorised ones (`TAX_NOT_AUTHORISED`), a tax it computes differently, and an unsupported tax or unit. Refusals before any create attempt release the money; an order with other units, taxes or totals is MISMATCH. v1 actions without these fields keep their digest. | `TestActionTaxVectors` (Go and TypeScript, `fixtures/vectors/action-tax-v1.json`), `TestR1TaxedOrderHoldsFullTotalAndCommits`, `TestR1TaxFieldsMustComeTogether`, `TestR1UnsupportedTaxConfigurationIsRefused`, `TestR1TaxRefusalAtCreateReleases`, `TestR1OrderWithDifferentUnitOrDeliveryIsMismatch`, real Odoo: the three `decision 1` subtests |
| 2 | Supplier and delivery protected | The engine reads the delivery address's protected revision from Odoo when it builds an action (`delivery_revision`) and again before it issues a permit. A change refuses evaluation and dispatch with `DELIVERY_REVISION_STALE` (409); amending builds a fresh action. Odoo compares it again inside the create and refuses (`DELIVERY_CHANGED`), releasing the money. The issuer cannot send a revision or a total. Revision counters only move forward: the engine's copy through `target.sync_supplier_revision`, Odoo's through its own trigger. | `TestR1DeliveryChangeNeedsFreshAuthorisation`, `TestR1DeliveryRevisionIsNotIssuerInput`, `TestR1DEF03SupplierRevisionComesFromTheDestination`, real Odoo: the two `decision 2` subtests |
| 3 | HELD money released only with proof | A RELEASE repair needs the recovery anchor to show a BLOCK and no create attempt (no `CREATE_INTENT`, no `CREATE_DONE`) for the reference. Without a BLOCK, confirmation fences first and reads the proof again. Any create attempt refuses RELEASE (`RELEASE_NOT_PROVEN`), at preparation and again at confirmation, even with a review and Aakhil's confirmation; COMMIT stays possible. The anchor now refuses any `CREATE_INTENT` after a BLOCK for the same reference, so the block is permanent even if Odoo is restored. Every HELD that today's settle path produces has a create attempt, so in practice RELEASE is refused for them. | `TestR1HeldWithCreateAttemptIsNeverReleased`, `TestR1ReleaseRecheckedAtConfirmation`, `TestR1ReleaseBlocksReferencePermanently`, `TestAnchorRoleSeparation` |
| 4 | Recovery anchor mandatory; three-person repairs | A connector requires the anchor (serve refuses to start; dispatch and readiness fail closed). Anchor repairs now go through the same three people as budget repairs: Uday prepares `ANCHOR_REPAIR` evidence, Kaif reviews it, Aakhil confirms with its `repair_id`; each must be a different person. An anchor repair moves no budget and never blocks new spending once confirmed. Writes still resume only through the resume gate. | `TestD5AnchorBehindNeedsConfirmedRepair`, `TestR1HTTPAnchorRepairNeedsThreePeople`, `TestR1RequiredAnchorCannotBeOmitted`, real Odoo: `case C` |
| 5 | Recovery timeout | Reconciliation stays resumable: it keeps 10 s back before its request deadline, stops between operations and reports `complete: false`; the next call continues. Progress and held money survive every stopped call. Measured against a slow anchor; see [r1-recovery-timeout.md](r1-recovery-timeout.md). | `TestR1DEF30ReconcileStopsAtItsBudgetAndResumes`, `TestR1DEF30SlowAnchorReconcileIsResumable`, `TestR1DEF30SlowAnchorCheckAndGate` |
| 6 | One tenant, one company | `STEELMATTER_TENANT` is required with a connector. Dispatch, settle, findings, repairs and the recovery endpoints refuse principals of any other tenant (403). One company per deployment (`STEELMATTER_COMPANY_REF`). Normal purchase writes in Odoo go only through Steelmatter's execution account (Kaif); an administrator bypass is reported as unprotected by the module's coverage report, not hidden. | `TestR1OneTenantPerDeployment` |
| 7 | Database credentials | The engine's backend database account is trusted: it has limited grants (no DDL, no DELETE, column-level UPDATE, signed fields immutable by trigger) and no agent ever receives it. A stolen backend credential is a documented limitation, below. | `TestHarnessExecutorCannotRunDDLOrReadMigrationHistory` and the immutability tests |
| 8 | Revocation and expiry | A revoked mandate, a changed control epoch or an expired mandate prevents every new send, from the revoke path and from the settle worker. Already-sent attempts are only looked up: a lost reply stays UNRESOLVED with its amount held until the destination answers, then commits or is fenced. | `TestR1ExpiredMandateNeverSends`, `TestR1ExpiryKeepsBudgetOfSentAttempt`, `TestR1RevokeAfterDispatchFencesUnsentOperation`, `TestR1WorkerNeverSendsAfterRevoke` |
| 9 | Human approval (T17–T19) | A tenant approval policy (`steelmatter.approval_policies`) sets the amount above which an action needs an approval. Entitled approvers (`steelmatter.approver_entitlements`) record decisions on the exact current action digest (`POST /v1/instructions/{id}/approvals`). Evaluation and simulation issue a permit only if the latest decision approved the same action digest, supplier revision, mandate revision and control epoch, and the approver is still entitled; otherwise DENY `APPROVAL_REQUIRED` or `APPROVAL_STALE`. The issuer and actor can never approve. Approvals reserve nothing; capacity is checked under the mandate lock, so concurrent approvals never exceed the allowance. Approvals are append-only for every role. | `TestT17ApprovalDriftIsRejected`, `TestT17SimulationNeedsApprovalToo`, `TestT18ApproverAuthority`, `TestT19ApprovalCapacityRace` |
| 10 | Release scope | `STEELMATTER_OBSERVE_ONLY=true` runs R1 without purchasing: dispatch is refused (`OBSERVE_ONLY`), an operation dispatched earlier is only looked up and fenced, and the observer, coverage, restore check and recovery keep running. R1 purchases go through HTTP and the TypeScript SDK. The Odoo module installs on a fresh database (`destinations/odoo/sandbox/up`, Kaif). MCP purchase creation is excluded from R1, below. | `TestR1ObserveOnlyNeverPurchases` |
| 11 | Evidence | Operation history is permanent: dispatches, anchor records (hash-chained, append-only) and approvals are never deleted or rewritten. Signed permits in the anchor's engine stream are readable only by the engine's recovery identity; the module and the anchor observer cannot read them. T34 results are recorded as Odoo-specific (the destination journal and `CREATE_DONE` record are Odoo module features). | `TestAnchorRoleSeparation`, `TestR1OdooEndToEnd` (T34 case E) |

## New configuration

| Setting | Meaning |
|---|---|
| `STEELMATTER_TENANT` | The one tenant this deployment serves. Required with `STEELMATTER_CONNECTOR=odoo`. |
| `STEELMATTER_OBSERVE_ONLY` | `true` to observe Odoo without purchasing (rollout step before purchasing). Default `false`. |
| `steelmatter.approval_policies` | One row per tenant: `approval_above_minor`. No row: no approval needed. Managed by the database administrator. |
| `steelmatter.approver_entitlements` | Who may approve (`enabled`). Set `enabled = false` to remove approval rights; earlier approvals by that person stop counting. |

Migration 0017 adds the approval tables. The anchor function change (permanent
BLOCK) applies to new anchors automatically; upgrade an existing one once with
`steelmatter anchor-upgrade` (records untouched, safe to repeat).

## API changes

- `ProposedAction` accepts `tax_minor`; `Line` accepts `unit_id` and `tax_ids`.
  The returned action may carry `amount_untaxed_minor`, `tax_minor` and
  `delivery_revision`.
- New error code `DELIVERY_REVISION_STALE` (409); new deny reasons
  `APPROVAL_REQUIRED` and `APPROVAL_STALE`.
- New endpoint `POST /v1/instructions/{instruction_id}/approvals`
  (`recordApproval` in the SDK).
- `prepareRepair` accepts `ANCHOR_REPAIR`; RELEASE can be refused with 403
  `RELEASE_NOT_PROVEN`.
- `confirmAnchorRepair` takes `{repair_id}` instead of `{evidence_ref}` and
  returns `repair_id`.

## Limitations (decision 7) and excluded scope (decision 10)

- **Backend credential compromise.** Whoever holds the engine's database
  credential can act as the engine: issue reservations, record dispatches and
  read signed permits. The anchor's hash chain and its separate roles detect a
  rewrite of history, and Odoo still refuses an order the module cannot match
  to a valid create, but a stolen credential is not prevented. Treat it as a
  trusted secret; no agent or MCP client ever receives it (agents hold only
  their own bearer token).
- **Hardening tracked for after R1:** move the credential to a secret
  manager with rotation; separate the worker and API database roles; mTLS to
  the database and the anchor; alerting on anchor writes outside the engine
  role; an HSM or KMS for the signing keys.
- **MCP purchase creation is excluded from R1.** The MCP adapter
  (`adapters/mcp`) has no dispatch tool; its admit tool reaches only the R0
  synthetic target. Odoo purchases are created only through the HTTP API and
  the SDK.
- **T34 is Odoo-specific.** Its evidence depends on the Odoo module's journal
  and restore-proof records; another destination needs its own T34 run.
