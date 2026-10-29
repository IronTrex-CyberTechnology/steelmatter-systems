package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// Settle advances one dispatched R1 operation by one connector round trip
// and records what the destination proved (lookup-first recovery):
//
//   - PENDING: execute the stored action with the same op_ref.
//   - UNRESOLVED: lookup; if the order is not found now, fence.
//   - COMMITTED, FENCED, MISMATCH: final here; returned unchanged.
//
// Allowance moves (D2, decided 28 Oct 2026):
//
//   - unresolved -> committed only when the destination returns a confirmed
//     order whose details match the approved action (CREATED, EXISTS, FOUND);
//   - unresolved -> available only when the destination proves no order:
//     FENCED or a structured module refusal, with the permanent block
//     committed and no earlier create attempt (Aakhil, 28 Oct 2026);
//   - a block over an earlier create attempt is HELD: the earlier outcome is
//     not established (an old backup may have erased a real order), so
//     blocking future attempts alone cannot release it (D5 v3 case 1);
//   - everything else (UNKNOWN, NOT_FOUND_NOW alone, UNAVAILABLE, restore
//     suspected, a differing order) leaves the amount held.
//
// A committed operation is never fenced: Settle only fences UNRESOLVED ones
// (D5 rule 7). Settle is safe to call concurrently; the first transaction to
// record a transition wins and the others return the stored state.
func (e *Engine) Settle(ctx context.Context, tenant, opRef string, c connector.Connector) (*DispatchResult, error) {
	r, _, err := e.settle(ctx, tenant, opRef, c)
	return r, err
}

// settle is Settle that also reports whether this call recorded the
// transition (false when the operation was already final or another settler
// recorded it first), so concurrent reconciliations report only their own
// work (R1-P4-DEF-01).
func (e *Engine) settle(ctx context.Context, tenant, opRef string, c connector.Connector) (*DispatchResult, bool, error) {
	if e.Cfg.RequireRecoveryAnchor && e.Anchor == nil {
		return nil, false, fault("SERVICE_UNAVAILABLE", "recovery anchor is required")
	}
	d, err := e.loadDispatchForSettle(ctx, tenant, opRef)
	if err != nil {
		return nil, false, err
	}
	if d == nil {
		return nil, false, fault("NOT_FOUND", "operation not found")
	}
	if d.status != DispatchPending && d.status != DispatchUnresolved { // COMMITTED, FENCED, MISMATCH, HELD
		r, err := e.GetDispatch(ctx, tenant, opRef)
		return r, false, err
	}
	// With the anchor: while writes are off (restore detected, or this
	// engine's generation is old, or the anchor cannot be read) nothing new
	// is created; a PENDING operation goes to lookup and fence instead, and
	// the fence skips the sequence gate because the restore is being
	// reconciled (sm_create still checks BLOCK whatever the sequence).
	writes := true
	if e.Anchor != nil {
		on, _, werr := e.writesOn(ctx)
		writes = werr == nil && on
		if writes {
			if d.minSeq, err = e.Anchor.ExpectedMinSeq(ctx, d.companyRef); err != nil {
				return nil, false, dependency("recovery anchor", err)
			}
		}
	}
	// D4 (R1-DEF-01): once the mandate is revoked, or its control epoch has
	// moved since dispatch, a PENDING operation is treated as sent: lookup
	// and fence only. Nothing new is ever created after a revoke.
	var res connector.Result
	if d.status == DispatchPending && writes && d.mandateLive {
		res, err = c.Execute(ctx, d.connectorAction())
	} else {
		res, err = c.Lookup(ctx, opRef)
		if err == nil && res.Outcome == connector.NotFoundNow {
			res, err = c.Fence(ctx, opRef, d.minSeq)
		}
	}
	if err != nil {
		return nil, false, dependency("destination", err)
	}
	if res.OpRef != "" && res.OpRef != opRef {
		return nil, false, fmt.Errorf("connector answered for %s, asked for %s", res.OpRef, opRef)
	}
	if err := failpoint.Inject(ctx, failpoint.SettleBeforeRecord); err != nil {
		return nil, false, err
	}
	if res, err = e.checkProofOfNoOrder(ctx, d, res); err != nil {
		return nil, false, err
	}
	changed, err := e.recordOutcome(ctx, d, res)
	if err != nil {
		return nil, false, err
	}
	r, err := e.GetDispatch(ctx, tenant, opRef)
	return r, changed, err
}

type dispatchRow struct {
	tenant, opRef, mandateID, companyRef, permitDigest, status string
	amount, destSeq, minSeq                                    int64
	action                                                     contract.Action
	mandateLive                                                bool // mandate ACTIVE at the dispatch's control epoch
}

// checkProofOfNoOrder (R1-DEF-09): the destination's reply alone never
// releases money. When a reply claims FENCED or a refusal with the block
// committed and no earlier create, the engine reads the anchor's destination
// stream itself (written by the module's own identity, outside the
// connector's reply path) before releasing:
//
//   - a CREATE_INTENT or CREATE_DONE for this op_ref means a create was
//     attempted: the operation is HELD, never released;
//   - no BLOCK record means the claimed block is not proven: the operation
//     stays UNRESOLVED and the fence is retried;
//   - an unreadable anchor is not proof either: UNRESOLVED.
//
// Without an anchor (R0-style deployments and stub tests) the reply flags
// are used as before; R1 with a real connector always has an anchor.
func (e *Engine) checkProofOfNoOrder(ctx context.Context, d *dispatchRow, r connector.Result) (connector.Result, error) {
	if e.Anchor == nil || !r.ProvesNoOrder() {
		return r, nil
	}
	p, err := e.Anchor.ProofFor(ctx, d.companyRef, d.opRef)
	switch {
	case err != nil:
		r.BlockCommitted = nil
		r.Reason = "anchor proof unreadable: " + err.Error()
	case p.CreateIntent || p.CreateDone != nil:
		yes := true
		r.PriorCreate = &yes
		r.Reason = "anchor records a create attempt; the reply claimed none"
	case !p.Blocked:
		r.BlockCommitted = nil
		r.Reason = "anchor has no BLOCK record for the claimed fence"
	}
	return r, nil
}

func (e *Engine) loadDispatchForSettle(ctx context.Context, tenant, opRef string) (*dispatchRow, error) {
	d := dispatchRow{tenant: tenant, opRef: opRef}
	var canonical []byte
	err := e.Pool.QueryRow(ctx, `SELECT d.mandate_id, d.company_ref, d.permit_digest, d.status, d.amount_minor, COALESCE(d.destination_seq, 0), d.action_canonical,
		m.status = 'ACTIVE' AND m.control_epoch = d.control_epoch
		FROM steelmatter.dispatches d JOIN steelmatter.mandates m ON m.tenant_id = d.tenant_id AND m.mandate_id = d.mandate_id
		WHERE d.tenant_id = $1 AND d.op_ref = $2`, tenant, opRef).
		Scan(&d.mandateID, &d.companyRef, &d.permitDigest, &d.status, &d.amount, &d.destSeq, &canonical, &d.mandateLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classify(err)
	}
	if err := json.Unmarshal(canonical, &d.action); err != nil {
		return nil, fmt.Errorf("dispatch %s: stored action: %w", opRef, err)
	}
	return &d, nil
}

func (d *dispatchRow) connectorAction() connector.Action {
	return toConnectorAction(d.opRef, d.companyRef, d.permitDigest, &d.action, d.minSeq)
}

// toConnectorAction maps the stored, authorised action to the canonical
// action the connector sends (D1 v1.2). Nothing caller-supplied is added.
func toConnectorAction(opRef, companyRef, permitDigest string, a *contract.Action, expectedMinSeq int64) connector.Action {
	lines := make([]connector.Line, len(a.Lines))
	for i, l := range a.Lines {
		lines[i] = connector.Line{ItemRef: l.ItemID, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor}
	}
	return connector.Action{OpRef: opRef, ActionRevision: a.ActionRevision, CompanyRef: companyRef,
		DeliveryRef: a.DeliveryID, SupplierRef: a.SupplierID,
		SupplierRevision: a.SupplierRevision, Lines: lines, AmountMinor: a.AmountMinor, Currency: a.Currency,
		PermitDigest: permitDigest, ExpectedMinSeq: expectedMinSeq}
}

// matches reports whether a destination order is a confirmed order with
// exactly the approved details (Aakhil's D1 decision: success only when the
// destination confirms the order and its details match).
func (d *dispatchRow) matches(o *connector.Order) bool {
	if o == nil || o.State != connector.StateConfirmed || o.CompanyRef != d.companyRef || o.DeliveryRef != d.action.DeliveryID ||
		o.AmountMinor != d.action.AmountMinor || o.SupplierRevision != d.action.SupplierRevision ||
		o.Currency != d.action.Currency || o.PermitDigest != d.permitDigest || o.SupplierRef != d.action.SupplierID ||
		len(o.Lines) != len(d.action.Lines) {
		return false
	}
	for i, l := range d.action.Lines {
		ol := o.Lines[i]
		if ol.ItemRef != l.ItemID || ol.Quantity != l.Quantity || ol.UnitPriceMinor != l.UnitPriceMinor {
			return false
		}
	}
	return true
}

// next decides the new status and allowance move for a destination result.
func (d *dispatchRow) next(r connector.Result) (status, event string) {
	switch {
	case r.CreateDone && r.Destination != nil:
		// The destination's CREATE_DONE record establishes the earlier
		// create, even when a restore erased the order (D5 v3 case E).
		if d.matches(r.Destination) {
			return DispatchCommitted, "operation.committed"
		}
		return DispatchMismatch, "operation.mismatch"
	case r.Outcome == connector.Created || r.Outcome == connector.Exists || r.Outcome == connector.Found:
		if d.matches(r.Destination) {
			return DispatchCommitted, "operation.committed"
		}
		return DispatchMismatch, "operation.mismatch"
	case r.Outcome == connector.Mismatch:
		return DispatchMismatch, "operation.mismatch"
	case r.ProvesNoOrder():
		return DispatchFenced, "operation.fenced"
	case r.BlockedOverPriorCreate():
		return DispatchHeld, "operation.held"
	default:
		// Includes a block not yet committed durably, or proof flags that
		// are unknown (restore-proof record unreadable): retry the fence.
		// UNKNOWN, NOT_FOUND_NOW, UNAVAILABLE, REFUSED_RESTORE_SUSPECTED or an
		// unrecognised value: the order may exist, so hold.
		return DispatchUnresolved, "operation.unresolved"
	}
}

func (e *Engine) recordOutcome(ctx context.Context, d *dispatchRow, r connector.Result) (bool, error) {
	status, event := d.next(r)
	eventID := newID("evt")
	order := ""
	if r.Destination != nil {
		order = r.Destination.ID
	}
	changed := false
	err := e.inTx(ctx, d.opRef, func(tx pgx.Tx) error {
		changed = false
		// Lock order: mandate, then the dispatch row.
		if _, err := lockMandate(ctx, tx, d.tenant, d.mandateID, "FOR UPDATE"); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2 FOR UPDATE`,
			d.tenant, d.opRef).Scan(&current); err != nil {
			return err
		}
		if current != d.status {
			return nil // another settler recorded a transition first
		}
		var move string
		switch status {
		case DispatchCommitted:
			move = `UPDATE steelmatter.mandates SET unresolved_minor = unresolved_minor - $3, committed_minor = committed_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`
		case DispatchFenced:
			move = `UPDATE steelmatter.mandates SET unresolved_minor = unresolved_minor - $3, available_minor = available_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`
		}
		if move != "" {
			if _, err := tx.Exec(ctx, move, d.tenant, d.mandateID, d.amount); err != nil {
				return err
			}
		}
		var facts []byte
		if status == DispatchCommitted {
			facts, _ = json.Marshal(r.Destination)
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.dispatches SET status = $3, last_outcome = $4, attempts = attempts + 1,
			destination_order = COALESCE(NULLIF($5, ''), destination_order), destination_seq = GREATEST(COALESCE(destination_seq, 0), $6),
			destination_facts = COALESCE(destination_facts, $7::jsonb), updated_at = clock_timestamp(), settled_at = CASE WHEN $3 IN ('COMMITTED', 'FENCED') THEN clock_timestamp() END
			WHERE tenant_id = $1 AND op_ref = $2`, d.tenant, d.opRef, status, string(r.Outcome), order, r.Seq, facts); err != nil {
			return err
		}
		if err := e.anchorOutcome(ctx, anchor.OutcomeRecord{Tenant: d.tenant, OpRef: d.opRef, Destination: d.companyRef, Outcome: string(r.Outcome),
			Status: status, Move: moveFor(status), DestinationOrder: order, DestinationSeq: r.Seq, Incarnation: r.Incarnation,
			BlockCommitted: r.BlockCommitted, PriorCreate: r.PriorCreate, CreateDone: r.CreateDone}); err != nil {
			return err
		}
		if status != d.status {
			payload, _ := json.Marshal(map[string]any{"op_ref": d.opRef, "outcome": r.Outcome, "destination_order": order, "amount_minor": d.amount})
			if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
				VALUES ($1, $2, $3, $4, $5, clock_timestamp())`, eventID, d.tenant, event, d.opRef, payload); err != nil {
				return err
			}
			changed = true
		}
		return nil
	})
	return changed && err == nil, err
}
