package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
)

// R1 execution values for a dispatched operation (draft 7, status model).
// Only DispatchCommitted is success.
const (
	DispatchPending    = "PENDING"
	DispatchUnresolved = "UNRESOLVED"
	DispatchCommitted  = "COMMITTED"
	DispatchFenced     = "FENCED"
	DispatchMismatch   = "MISMATCH"
	DispatchHeld       = "HELD"
)

// DispatchResult is the stored state of an R1 dispatched operation.
type DispatchResult struct {
	OpRef            string `json:"op_ref"`
	InstructionID    string `json:"instruction_id"`
	ActionRevision   int64  `json:"action_revision"`
	ActionDigest     string `json:"action_digest"`
	Execution        string `json:"execution"`
	LastOutcome      string `json:"last_outcome,omitempty"`
	DestinationOrder string `json:"destination_order,omitempty"`
	Created          bool   `json:"-"`
}

// Dispatch records R1 dispatch intent for the exact permitted action. It runs
// the same authorisation gate as Admit, then in one transaction consumes the
// permit, records the operation (op_ref = the permit's operation ID), freezes
// the action revision and moves the amount reserved -> unresolved (D2 move 3).
// From commit onward the order may exist at the destination, so nothing here
// can release the amount again; only Settle can, on proof from the
// destination.
//
// This transaction is also the enforceable revocation boundary (D4): it locks
// the mandate and refuses if the mandate is revoked or its epoch changed. A
// revocation that commits later finds the permit consumed and leaves the
// unresolved amount alone.
//
// An identical repeat returns the stored state (Created false); a different
// action for the same instruction conflicts.
func (e *Engine) Dispatch(ctx context.Context, p auth.Principal, actionJSON []byte, permitJWS string) (*DispatchResult, error) {
	if e.Cfg.CompanyRef == "" {
		return nil, fault("SERVICE_UNAVAILABLE", "no destination company is configured for dispatch")
	}
	a, err := e.preAdmit(p, actionJSON, permitJWS)
	if err != nil {
		return nil, err
	}
	permit, action := a.permit, a.action
	var res *DispatchResult
	eventID := newID("evt")
	err = e.inTx(ctx, permit.OperationID, func(tx pgx.Tx) error {
		res = nil
		if err := e.lockForAdmission(ctx, tx, p, a); err != nil {
			return err
		}
		if r0, err := loadOperationByInstruction(ctx, tx, p.Tenant, permit.InstructionID, e.Cfg.AdapterID); err != nil {
			return err
		} else if r0 != nil {
			return fault("ACTION_CHANGED", "instruction was already committed by synthetic admission")
		}
		stored, err := loadDispatchByInstruction(ctx, tx, p.Tenant, permit.InstructionID)
		if err != nil {
			return err
		}
		if stored != nil {
			if stored.ActionDigest != a.actionDigest {
				return fault("ACTION_CHANGED", "instruction already dispatched a different action")
			}
			res = stored
			return nil
		}
		if err := e.verifyLiveAdmission(ctx, tx, p, a); err != nil {
			return err
		}
		is := a.issuance

		tag, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'CONSUMED', consumed_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2 AND status = 'ACTIVE' AND clock_timestamp() < expires_at`, p.Tenant, permit.PermitID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fault("PERMIT_EXPIRED", "permit expired during dispatch")
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp())`).Scan(&now); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{
			"op_ref": permit.OperationID, "instruction_id": action.InstructionID, "action_digest": a.actionDigest, "amount_minor": is.Amount,
		})
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO steelmatter.dispatches (tenant_id, op_ref, instruction_id, action_revision, mandate_id, company_ref, permit_id, control_epoch,
			  action_canonical, action_digest, permit_digest, amount_minor, status, created_at, updated_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'PENDING', $13, $13)`,
				[]any{p.Tenant, permit.OperationID, action.InstructionID, action.ActionRevision, permit.MandateID, e.Cfg.CompanyRef, permit.PermitID,
					permit.ControlEpoch, a.canonical, a.actionDigest, a.verified.Digest, is.Amount, now}},
			// Freeze the revision: no amendment once the order may exist.
			{`UPDATE steelmatter.action_revisions SET status = 'COMMITTED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
				[]any{p.Tenant, action.InstructionID, action.ActionRevision}},
			{`UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, unresolved_minor = unresolved_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`,
				[]any{p.Tenant, permit.MandateID, is.Amount}},
			{`INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at) VALUES ($1, $2, 'operation.dispatched', $3, $4, $5)`,
				[]any{eventID, p.Tenant, permit.OperationID, payload, now}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		res = &DispatchResult{OpRef: permit.OperationID, InstructionID: action.InstructionID, ActionRevision: action.ActionRevision,
			ActionDigest: a.actionDigest, Execution: DispatchPending, Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

const dispatchCols = `op_ref, instruction_id, action_revision, action_digest, status, COALESCE(last_outcome, ''), COALESCE(destination_order, '')`

func scanDispatch(row pgx.Row) (*DispatchResult, error) {
	var r DispatchResult
	err := row.Scan(&r.OpRef, &r.InstructionID, &r.ActionRevision, &r.ActionDigest, &r.Execution, &r.LastOutcome, &r.DestinationOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func loadDispatchByInstruction(ctx context.Context, q pgx.Tx, tenant, instructionID string) (*DispatchResult, error) {
	return scanDispatch(q.QueryRow(ctx, `SELECT `+dispatchCols+` FROM steelmatter.dispatches WHERE tenant_id = $1 AND instruction_id = $2`,
		tenant, instructionID))
}

// GetDispatch returns the stored state of an R1 operation, or nil.
func (e *Engine) GetDispatch(ctx context.Context, tenant, opRef string) (*DispatchResult, error) {
	r, err := scanDispatch(e.Pool.QueryRow(ctx, `SELECT `+dispatchCols+` FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2`, tenant, opRef))
	if err != nil {
		return nil, classify(err)
	}
	return r, nil
}
