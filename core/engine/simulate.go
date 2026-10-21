package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
)

// SimulationResult is a nonbinding evaluation outcome (document 03).
type SimulationResult struct {
	Simulation bool   `json:"simulation"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
}

// Simulate answers "would evaluate issue a permit now?" for an operator,
// without creating a permit, reservation or target write (T30). It runs the
// same checks as Evaluate inside a READ ONLY transaction, so PostgreSQL
// itself refuses any write. Conflicts that would make Evaluate fail (stale
// revision, committed instruction, changed supplier, lost entitlement) are
// returned as the same errors.
func (e *Engine) Simulate(ctx context.Context, p auth.Principal, instructionID string, revision int64) (*SimulationResult, error) {
	if p.Kind != auth.Operator {
		return nil, fault("FORBIDDEN", "only an operator can run a simulation")
	}
	var res *SimulationResult
	err := pgx.BeginTxFunc(ctx, e.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
			return err
		}
		// Clock health blocks issuance in evaluate, so it blocks the answer here.
		if err := e.checkClock(ctx, tx); err != nil {
			return err
		}
		var mandateID, actor, sourceSystem string
		var current int64
		err := tx.QueryRow(ctx, `SELECT mandate_id, actor_id, source_system, current_revision FROM steelmatter.instructions
			WHERE tenant_id = $1 AND instruction_id = $2`, p.Tenant, instructionID).Scan(&mandateID, &actor, &sourceSystem, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("NOT_FOUND", "instruction not found")
		}
		if err != nil {
			return err
		}
		m, err := lockMandate(ctx, tx, p.Tenant, mandateID, "") // plain read: no locks in a read-only transaction
		if err != nil {
			return err
		}
		// Entitlement as the assigned actor would have it at evaluation.
		if err := currentEntitlement(ctx, tx, auth.Principal{ID: actor, Tenant: p.Tenant, Kind: auth.Actor}, sourceSystem, mandateID, m, actor); err != nil {
			return err
		}
		var aStatus, supplier, delivery string
		var supplierRev, amount int64
		err = tx.QueryRow(ctx, `SELECT status, supplier_id, supplier_revision, delivery_id, amount_minor FROM steelmatter.action_revisions
			WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`, p.Tenant, instructionID, revision).
			Scan(&aStatus, &supplier, &supplierRev, &delivery, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("REVISION_STALE", "action revision %d does not exist", revision)
		}
		if err != nil {
			return err
		}
		live, err := scanIssuance(tx.QueryRow(ctx, `SELECT `+issuanceCols+` FROM steelmatter.issuances
			WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 AND status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED')`,
			p.Tenant, instructionID, revision))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var liveSupplierRev int64
		var supplierStatus string
		if err := tx.QueryRow(ctx, `SELECT revision, status FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2`,
			p.Tenant, supplier).Scan(&liveSupplierRev, &supplierStatus); err != nil {
			return err
		}
		if aStatus == "COMMITTED" || (live != nil && live.Status == "CONSUMED") {
			return fault("ALREADY_COMMITTED", "instruction is already committed")
		}
		if aStatus != "CURRENT" || current != revision {
			return fault("REVISION_STALE", "action revision %d is not current (%d)", revision, current)
		}
		deny := func(reason string) error {
			res = &SimulationResult{Simulation: true, Decision: "DENY", Reason: reason}
			return nil
		}
		switch {
		case m.Status != "ACTIVE":
			return deny("AUTHORITY_REVOKED")
		case m.Expired:
			return deny("MANDATE_EXPIRED")
		case !contains(m.Supplier, supplier):
			return deny("SUPPLIER_NOT_PERMITTED")
		case !contains(m.Delivery, delivery):
			return deny("DELIVERY_NOT_PERMITTED")
		}
		if liveSupplierRev != supplierRev || supplierStatus != "ACTIVE" {
			return fault("SUPPLIER_REVISION_STALE", "protected supplier revision changed; amend and re-evaluate")
		}
		// The same branches as evaluate: return or complete the live issuance,
		// otherwise count what releasing it would return to the allowance.
		var released int64
		if live != nil {
			if (live.Status == "ACTIVE" || live.Status == "UNSIGNED") && !live.Expired && live.Epoch == m.Epoch {
				res = &SimulationResult{Simulation: true, Decision: "PERMIT_WOULD_ISSUE"}
				return nil
			}
			released = live.Amount
		}
		var available int64
		var mandateUsable bool
		if err := tx.QueryRow(ctx, `SELECT available_minor, date_trunc('second', expires_at) > date_trunc('second', clock_timestamp())
			FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID).Scan(&available, &mandateUsable); err != nil {
			return err
		}
		if available > contract.MaxSafeInteger-released {
			return fmt.Errorf("allowance arithmetic overflow")
		}
		if available+released < amount {
			return deny("CAPACITY_EXHAUSTED")
		}
		if !mandateUsable {
			return deny("MANDATE_EXPIRED") // expires before a permit could be used
		}
		res = &SimulationResult{Simulation: true, Decision: "PERMIT_WOULD_ISSUE"}
		return nil
	})
	if err != nil {
		return nil, classify(err)
	}
	return res, nil
}

// AdapterAssurance describes what the destination adapter supports.
type AdapterAssurance struct {
	AdapterID           string   `json:"adapter_id"`
	Version             string   `json:"version"`
	Simulation          bool     `json:"simulation"`
	SupportedOperations []string `json:"supported_operations"`
	SupportedPaths      []string `json:"supported_paths"`
	Limitations         []string `json:"limitations"`
	TestReferences      []string `json:"test_references"`
}

// Assurance returns the adapter statement to any authenticated reader kind
// (actor, issuer, owner, observer, operator) of the tenant. An unknown
// adapter ID is NOT_FOUND.
func (e *Engine) Assurance(ctx context.Context, p auth.Principal, adapterID string) (*AdapterAssurance, error) {
	switch p.Kind {
	case auth.Actor, auth.Issuer, auth.Owner, auth.Observer, auth.Operator:
	default:
		return nil, fault("FORBIDDEN", "principal kind may not read adapter assurance")
	}
	if adapterID != e.Cfg.AdapterID {
		return nil, fault("NOT_FOUND", "adapter not found")
	}
	return &AdapterAssurance{
		AdapterID:           e.Cfg.AdapterID,
		Version:             e.Cfg.Version,
		Simulation:          true,
		SupportedOperations: []string{"purchase_order.issue"},
		SupportedPaths: []string{
			"commit: order, lines, obligation, accounting, permit consumption, result and outbox in one PostgreSQL transaction",
			"replay: an identical instruction/action returns the stored result without a new effect",
			"lookup: GET /v1/operations/{id} by operation_id after a lost response",
			"readback: read-only observer role compares every material field (MATCH, MISMATCH, INSUFFICIENT_EVIDENCE)",
			"evidence: signed mandate, permit, execution and observation records with linkage",
		},
		Limitations: []string{
			"Synthetic destination in the same PostgreSQL database; not a real ERP or supplier system.",
			"No external dispatch: OUTCOME_UNKNOWN and external reconciliation are R1.",
			"No cancellation, partial execution, refunds, taxes, FX, discounts or split mandates.",
			"One organisation, INR only, integer paise.",
			"The observer shares host and database administrators with the executor; observation is not independent attestation.",
		},
		TestReferences: []string{
			"T03 material tamper", "T04 stale supplier", "T06 audience substitution", "T09 replay", "T13 lost HTTP response",
			"T14 precommit crash", "T15 postcommit crash", "T24 observer mismatch", "T30 simulation isolation", "T39 obligation linkage",
		},
	}, nil
}
