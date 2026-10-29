package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
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
	// Flag marks a committed operation whose order changed at the
	// destination afterwards: ORDER_MISSING_AFTER_RESTORE,
	// ORDER_DELETED_AT_DESTINATION or ORDER_CHANGED_AT_DESTINATION. The
	// amount stays committed; an operator records a finding.
	Flag    string `json:"flag,omitempty"`
	Created bool   `json:"-"`
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
	if err := e.ServesTenant(p.Tenant); err != nil {
		return nil, err
	}
	if e.Cfg.ObserveOnly {
		return nil, fault("FORBIDDEN", "OBSERVE_ONLY: this deployment observes the destination and does not purchase; nothing was dispatched")
	}
	a, err := e.preAdmit(p, actionJSON, permitJWS)
	if err != nil {
		return nil, err
	}
	permit, action := a.permit, a.action
	// R1-DEF-12: a dispatch that no connector can ever settle would strand
	// the amount as unresolved. Refuse before anything is consumed or moved.
	if e.Connector == nil {
		if d, gerr := e.GetDispatch(ctx, p.Tenant, permit.OperationID); gerr == nil && d != nil && d.ActionDigest == a.actionDigest {
			return d, nil
		}
		return nil, fault("SERVICE_UNAVAILABLE", "no destination connector is configured; nothing was dispatched")
	}
	// The delivery check is never optional: an action without a delivery
	// address revision is refused before anything is consumed.
	if err := e.requireDeliveryRevision(action); err != nil {
		return nil, err
	}
	if err := e.requireWrites(ctx); err != nil {
		if d, gerr := e.GetDispatch(ctx, p.Tenant, permit.OperationID); gerr == nil && d != nil && d.ActionDigest == a.actionDigest {
			return d, nil // an identical repeat of a recorded dispatch still returns its state
		}
		return nil, err
	}
	if err := e.checkReadiness(ctx, p, a); err != nil {
		return nil, err
	}
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
		// R1-DEF-06: a retry after a dispatch whose engine commit failed
		// finds that attempt's intent in the anchor. It is the same
		// authorised request, so the same intent (and admission time) is
		// reused and the append is an idempotent repeat.
		admitted := now
		if e.Anchor != nil {
			prior, err := e.priorIntent(ctx, p.Tenant, permit.OperationID)
			if err != nil {
				return anchorFault(err)
			}
			if prior != nil && prior.AdmittedAt != "" && prior.PermitDigest == a.verified.Digest && prior.ActionDigest == a.actionDigest &&
				prior.AmountMinor == is.Amount {
				if t, err := time.Parse(time.RFC3339, prior.AdmittedAt); err == nil {
					admitted = t
				}
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"op_ref": permit.OperationID, "instruction_id": action.InstructionID, "action_digest": a.actionDigest, "amount_minor": is.Amount,
		})
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO steelmatter.dispatches (tenant_id, op_ref, instruction_id, action_revision, mandate_id, company_ref, permit_id, control_epoch,
			  action_canonical, action_digest, permit_digest, amount_minor, status, created_at, updated_at, admitted_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'PENDING', $13, $13, $14)`,
				[]any{p.Tenant, permit.OperationID, action.InstructionID, action.ActionRevision, permit.MandateID, e.Cfg.CompanyRef, permit.PermitID,
					permit.ControlEpoch, a.canonical, a.actionDigest, a.verified.Digest, is.Amount, now, admitted}},
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
		// Record the intent in the recovery anchor, last, just before commit
		// (D5 mechanism step 1). No anchor record, no dispatch; and none can be
		// appended once writes are off or this engine's generation is old.
		if e.Anchor != nil {
			ir := anchor.IntentRecord{AdmittedAt: ts(admitted), Tenant: p.Tenant, OpRef: permit.OperationID, InstructionID: action.InstructionID,
				ActionRevision: action.ActionRevision, MandateID: permit.MandateID, ControlEpoch: permit.ControlEpoch, CompanyRef: e.Cfg.CompanyRef,
				PermitID: permit.PermitID, PermitJWS: permitJWS, PermitDigest: a.verified.Digest, ActionCanonical: string(a.canonical),
				ActionDigest: a.actionDigest, AmountMinor: is.Amount, Currency: action.Currency, SupplierRef: action.SupplierID,
				SupplierRevision: action.SupplierRevision, DeliveryRef: action.DeliveryID}
			if _, err := e.Anchor.Append(ctx, anchor.DispatchIntent, p.Tenant, permit.OperationID, ir, e.anchorGen.Load()); err != nil {
				return anchorFault(err)
			}
		}
		if err := failpoint.Inject(ctx, failpoint.DispatchBeforeCommit); err != nil {
			return err
		}
		res = &DispatchResult{OpRef: permit.OperationID, InstructionID: action.InstructionID, ActionRevision: action.ActionRevision,
			ActionDigest: a.actionDigest, Execution: DispatchPending, Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res != nil && res.Created {
		e.markDispatched(ctx, p.Tenant, res.OpRef, e.Cfg.CompanyRef)
	}
	return res, nil
}

// priorIntent returns the anchor's DISPATCH_INTENT for (tenant, opRef), if any.
func (e *Engine) priorIntent(ctx context.Context, tenant, opRef string) (*anchor.IntentRecord, error) {
	recs, err := e.Anchor.RecordsFor(ctx, tenant, opRef)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.Type == anchor.DispatchIntent {
			var ir anchor.IntentRecord
			if err := json.Unmarshal(r.Payload, &ir); err != nil {
				return nil, err
			}
			return &ir, nil
		}
	}
	return nil, nil
}

const dispatchCols = `op_ref, instruction_id, action_revision, action_digest, status, COALESCE(last_outcome, ''), COALESCE(destination_order, ''), COALESCE(flag, '')`

func scanDispatch(row pgx.Row) (*DispatchResult, error) {
	var r DispatchResult
	err := row.Scan(&r.OpRef, &r.InstructionID, &r.ActionRevision, &r.ActionDigest, &r.Execution, &r.LastOutcome, &r.DestinationOrder, &r.Flag)
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

// checkReadiness asks the destination, read-only, whether it can take this
// action (D1 readiness). NOT_READY blocks dispatch before anything is
// consumed or moved, so the caller can retry with the same permit. READY is
// advisory only: the destination write itself re-checks everything. An
// identical repeat of a dispatch already recorded skips the check, so a
// destination outage never hides a stored result. No connector, no check.
func (e *Engine) checkReadiness(ctx context.Context, p auth.Principal, a *admission) error {
	if e.Connector == nil {
		return nil
	}
	if d, err := e.GetDispatch(ctx, p.Tenant, a.permit.OperationID); err != nil {
		return err
	} else if d != nil {
		return nil
	}
	r, err := e.Connector.Readiness(ctx, toConnectorAction(a.permit.OperationID, e.Cfg.CompanyRef, a.verified.Digest, a.action, 0))
	if err != nil {
		return dependency("destination", err)
	}
	switch {
	case r.Outcome == connector.Ready:
		return nil
	case r.Reason == connector.ReasonSupplierChanged:
		return fault("SUPPLIER_REVISION_STALE", "destination reports the protected supplier changed after authorisation; re-evaluation required")
	case r.Reason == connector.ReasonDeliveryChanged:
		return fault("DELIVERY_REVISION_STALE", "destination reports the delivery address changed after authorisation; amend and re-evaluate")
	case r.Reason == connector.ReasonDestinationUnavailable || r.Outcome != connector.NotReady:
		return fault("SERVICE_UNAVAILABLE", "destination is not reachable; nothing was dispatched")
	default:
		return fault("ADAPTER_UNSUPPORTED", "destination is not ready for this action (%s); nothing was dispatched", r.Reason)
	}
}
