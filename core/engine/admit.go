package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// TargetRef is a scoped reference to the synthetic destination record.
type TargetRef struct {
	AdapterID string `json:"adapter_id"`
	OrderID   string `json:"order_id"`
}

// OperationResult is the stored admission result.
type OperationResult struct {
	OperationID    string    `json:"operation_id"`
	InstructionID  string    `json:"instruction_id"`
	ActionRevision int64     `json:"action_revision"`
	ActionDigest   string    `json:"action_digest"`
	Execution      string    `json:"execution"`
	ObligationID   string    `json:"obligation_id"`
	TargetRef      TargetRef `json:"target_ref"`
	CommittedAt    string    `json:"committed_at"`
	Created        bool      `json:"-"`
}

func contractFault(err error) error {
	if ce, ok := err.(*contract.Error); ok {
		return fault(string(ce.Code), "%s", ce.Detail)
	}
	return err
}

// Admit presents the exact action and permit through the gate. It commits a
// new order, obligation, accounting move, permit consumption, result and
// outbox event in one transaction (201), or returns the stored result for an
// identical already-committed instruction/action (200), or rejects with no
// effect.
func (e *Engine) Admit(ctx context.Context, p auth.Principal, actionJSON []byte, permitJWS string) (*OperationResult, error) {
	a, err := e.preAdmit(p, actionJSON, permitJWS)
	if err != nil {
		return nil, err
	}
	permit, verified, action, actionDigest := a.permit, a.verified, a.action, a.actionDigest

	var res *OperationResult
	orderID, obligationID, executionID, eventID := newID("po"), newID("obl"), newID("exec"), newID("evt")
	err = e.inTx(ctx, permit.OperationID, func(tx pgx.Tx) error {
		res = nil
		if err := e.lockForAdmission(ctx, tx, p, a); err != nil {
			return err
		}

		// Identical already-committed operation: return the stored result,
		// even if the historical permit has since expired. Changed digest conflicts.
		stored, err := loadOperationByInstruction(ctx, tx, p.Tenant, permit.InstructionID, e.Cfg.AdapterID)
		if err != nil {
			return err
		}
		if stored != nil {
			if stored.ActionDigest != actionDigest {
				return fault("ACTION_CHANGED", "instruction already committed a different action")
			}
			res = stored
			return nil
		}

		if err := e.verifyLiveAdmission(ctx, tx, p, a); err != nil {
			return err
		}
		is := a.issuance

		var committedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp())`).Scan(&committedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id,
			supplier_revision, delivery_id, currency, amount_minor, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, 'INR', $8, $9)`,
			p.Tenant, orderID, action.InstructionID, permit.OperationID, action.SupplierID, action.SupplierRevision, action.DeliveryID,
			action.AmountMinor, committedAt); err != nil {
			if isUnique(err, "purchase_orders_one_per_instruction") {
				return fault("ACTION_CHANGED", "instruction already has an order")
			}
			return err
		}
		for _, l := range action.Lines {
			if _, err := tx.Exec(ctx, `INSERT INTO target.purchase_order_lines (tenant_id, order_id, item_id, quantity, unit_price_minor)
				VALUES ($1, $2, $3, $4, $5)`, p.Tenant, orderID, l.ItemID, l.Quantity, l.UnitPriceMinor); err != nil {
				return err
			}
		}
		if err := failpoint.Inject(ctx, failpoint.AdmissionAfterTargetInsert); err != nil {
			return err
		}
		exec := contract.Execution{
			SchemaVersion: contract.SchemaVersion, CryptoProfile: contract.CryptoProfile, ExecutionID: executionID,
			OperationID: permit.OperationID, TenantID: p.Tenant, InstructionID: action.InstructionID, ObligationID: obligationID,
			ActionDigest: actionDigest, PermitDigest: verified.Digest, TargetOrderID: orderID, Currency: contract.Currency,
			AmountMinor: action.AmountMinor, Status: "COMMITTED", CommittedAt: ts(committedAt),
		}
		execRaw, _ := json.Marshal(exec)
		execCanonical, execDigest, err := contract.CanonicalDigest(contract.KindExecution, execRaw)
		if err != nil {
			return err
		}
		eventPayload, _ := json.Marshal(map[string]any{
			"operation_id": permit.OperationID, "instruction_id": action.InstructionID, "order_id": orderID,
			"obligation_id": obligationID, "action_digest": actionDigest, "execution_digest": execDigest, "amount_minor": action.AmountMinor,
		})
		// Consume the permit only while it is still unexpired at write time;
		// zero rows means expiry passed during the transaction.
		tag, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'CONSUMED', consumed_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2 AND status = 'ACTIVE' AND clock_timestamp() < expires_at`, p.Tenant, permit.PermitID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fault("PERMIT_EXPIRED", "permit expired during admission")
		}
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO steelmatter.obligations (tenant_id, obligation_id, instruction_id, operation_id, action_digest, amount_minor, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`, []any{p.Tenant, obligationID, action.InstructionID, permit.OperationID, actionDigest, action.AmountMinor, committedAt}},
			{`INSERT INTO steelmatter.operations (tenant_id, operation_id, instruction_id, action_revision, permit_id, action_digest, status,
			  target_order_id, obligation_id, execution_canonical, execution_digest, committed_at)
			  VALUES ($1, $2, $3, $4, $5, $6, 'COMMITTED', $7, $8, $9, $10, $11)`,
				[]any{p.Tenant, permit.OperationID, action.InstructionID, action.ActionRevision, permit.PermitID, actionDigest, orderID, obligationID, execCanonical, execDigest, committedAt}},
			{`UPDATE steelmatter.action_revisions SET status = 'COMMITTED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
				[]any{p.Tenant, action.InstructionID, action.ActionRevision}},
			{`UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, committed_minor = committed_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`,
				[]any{p.Tenant, permit.MandateID, is.Amount}},
			{`INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at) VALUES ($1, $2, 'operation.committed', $3, $4, $5)`,
				[]any{eventID, p.Tenant, permit.OperationID, eventPayload, committedAt}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		res = &OperationResult{OperationID: permit.OperationID, InstructionID: action.InstructionID, ActionRevision: action.ActionRevision,
			ActionDigest: actionDigest, Execution: "COMMITTED", ObligationID: obligationID,
			TargetRef: TargetRef{AdapterID: e.Cfg.AdapterID, OrderID: orderID}, CommittedAt: ts(committedAt), Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.Created {
		// Placed directly after commit, before any further work, so crash
		// tests see the committed-but-unsignaled state deterministically.
		if err := failpoint.Inject(ctx, failpoint.AdmissionAfterCommit); err != nil {
			f := &Fault{Code: "SERVICE_UNAVAILABLE", Message: "response lost after commit", OperationID: res.OperationID}
			return nil, f
		}
		// Best effort; the evidence worker signs anything left pending.
		_ = e.SignExecution(ctx, p.Tenant, res.OperationID)
	}
	return res, nil
}

func loadOperationByInstruction(ctx context.Context, q pgx.Tx, tenant, instructionID, adapter string) (*OperationResult, error) {
	var r OperationResult
	var committedAt time.Time
	err := q.QueryRow(ctx, `SELECT operation_id, instruction_id, action_revision, action_digest, obligation_id, target_order_id, committed_at
		FROM steelmatter.operations WHERE tenant_id = $1 AND instruction_id = $2`, tenant, instructionID).
		Scan(&r.OperationID, &r.InstructionID, &r.ActionRevision, &r.ActionDigest, &r.ObligationID, &r.TargetRef.OrderID, &committedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Execution, r.TargetRef.AdapterID, r.CommittedAt = "COMMITTED", adapter, ts(committedAt)
	return &r, nil
}
