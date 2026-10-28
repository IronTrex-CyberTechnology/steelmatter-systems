package engine

import (
	"context"
	"encoding/json"
	"errors"

	"fmt"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
)

// UseAnchor connects the engine to the recovery anchor (D5 v3) and adopts
// its current write generation. From then on every dispatch records its
// intent in the anchor before committing, every destination result is
// recorded there, and dispatch is refused while writes are off or this
// engine's generation is old.
func (e *Engine) UseAnchor(ctx context.Context, c *anchor.Client) (anchor.WriteState, error) {
	e.Anchor = c
	return e.AdoptWriteState(ctx)
}

// AdoptWriteState loads the anchor's write generation into this engine. A
// process started before a restore keeps its old generation and is refused
// until it adopts the new one (at restart, or after the resume gate it ran).
func (e *Engine) AdoptWriteState(ctx context.Context) (anchor.WriteState, error) {
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return ws, err
	}
	e.anchorGen.Store(ws.Generation)
	return ws, nil
}

// WriteGeneration is the anchor generation this engine dispatches under.
func (e *Engine) WriteGeneration() int64 { return e.anchorGen.Load() }

// writesOn reports whether this engine may write new orders: writes are
// enabled in the anchor at this engine's generation. Without an anchor
// writes are on only for explicitly unanchored test configurations. A read failure is an error: callers fail closed.
func (e *Engine) writesOn(ctx context.Context) (bool, anchor.WriteState, error) {
	if e.Anchor == nil {
		if e.Cfg.RequireRecoveryAnchor {
			return false, anchor.WriteState{}, errors.New("recovery anchor is required")
		}
		return true, anchor.WriteState{Enabled: true}, nil
	}
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return false, ws, err
	}
	return ws.Enabled && ws.Generation == e.anchorGen.Load(), ws, nil
}

func (e *Engine) requireWrites(ctx context.Context) error {
	on, ws, err := e.writesOn(ctx)
	switch {
	case err != nil:
		return fault("SERVICE_UNAVAILABLE", "recovery anchor is unavailable; nothing was dispatched")
	case !on && !ws.Enabled:
		return fault("SERVICE_UNAVAILABLE", "writes are disabled (%s); nothing was dispatched", ws.Reason)
	case !on:
		return fault("SERVICE_UNAVAILABLE", "this process's write generation is old; restart it to adopt generation %d", ws.Generation)
	}
	return nil
}

// anchorFault maps an anchor append failure to a client-visible fault.
func anchorFault(err error) error {
	if errors.Is(err, anchor.ErrWritesDisabled) {
		return fault("SERVICE_UNAVAILABLE", "writes were disabled during dispatch; nothing was dispatched")
	}
	return fault("SERVICE_UNAVAILABLE", "recovery anchor is unavailable; nothing was dispatched")
}

func moveFor(status string) string {
	switch status {
	case DispatchCommitted:
		return "unresolved->committed"
	case DispatchFenced:
		return "unresolved->available"
	}
	return "none"
}

// anchorOutcome records a destination result and the resulting status in the
// anchor (mechanism step 2). It runs inside the engine transaction before
// commit, so the engine never records an outcome the anchor lacks.
func (e *Engine) anchorOutcome(ctx context.Context, rec anchor.OutcomeRecord) error {
	if e.Anchor == nil {
		return nil
	}
	if _, err := e.Anchor.Append(ctx, anchor.Outcome, rec.Tenant, rec.OpRef, rec, 0); err != nil {
		return fmt.Errorf("recovery anchor: %w", err)
	}
	return nil
}

// InitializeRecovery runs before HTTP admission or workers start. Restarts
// cannot inherit an enabled write switch without checking the restored stores.
// Recovery endpoints remain available when the gate fails, but writes stay off.
func (e *Engine) InitializeRecovery(ctx context.Context, c connector.Connector) (*GateReport, error) {
	if err := e.recoveryReady(c); err != nil {
		return nil, err
	}
	if _, err := e.Anchor.DisableWrites(ctx, "startup recovery check"); err != nil {
		return nil, err
	}
	return e.TryResume(ctx, c)
}

// guardMandateHistory MUST run after the mandate FOR UPDATE lock. Unlike the
// background detector it has no grace period: the lock excludes an in-flight
// append for this budget. It protects both new reservations and admission.
func (e *Engine) guardMandateHistory(ctx context.Context, tx pgx.Tx, tenant, mandate string) error {
	if e.Anchor == nil && !e.Cfg.RequireRecoveryAnchor {
		return nil
	}
	if err := e.requireWrites(ctx); err != nil {
		return err
	}
	recs, err := e.Anchor.RecordsForMandate(ctx, tenant, mandate)
	if err != nil {
		return anchorFault(err)
	}
	type history struct {
		intent                       *anchor.IntentRecord
		status, repair, repairStatus string
	}
	hist := map[string]*history{}
	for _, r := range recs {
		h := hist[r.OpRef]
		if h == nil {
			h = &history{}
			hist[r.OpRef] = h
		}
		switch r.Type {
		case anchor.DispatchIntent:
			var ir anchor.IntentRecord
			if err := json.Unmarshal(r.Payload, &ir); err != nil {
				return anchorFault(err)
			}
			h.intent = &ir
		case anchor.Outcome:
			var out anchor.OutcomeRecord
			if err := json.Unmarshal(r.Payload, &out); err != nil {
				return anchorFault(err)
			}
			h.status = out.Status
		case anchor.RepairConfirmed:
			var rc RepairConfirmRecord
			if err := json.Unmarshal(r.Payload, &rc); err != nil {
				return anchorFault(err)
			}
			if rc.RepairID != "" {
				h.repairStatus, h.repair = rc.ToStatus, rc.RepairID
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT op_ref, status, amount_minor, action_digest, permit_digest, COALESCE(repair_ref, '')
		FROM steelmatter.dispatches WHERE tenant_id = $1 AND mandate_id = $2`, tenant, mandate)
	if err != nil {
		return err
	}
	problem := ""
	for rows.Next() {
		var op, status, action, permit, repair string
		var amount int64
		if err := rows.Scan(&op, &status, &amount, &action, &permit, &repair); err != nil {
			rows.Close()
			return err
		}
		h := hist[op]
		if h == nil || h.intent == nil || h.intent.AmountMinor != amount || h.intent.ActionDigest != action || h.intent.PermitDigest != permit {
			problem = "engine and anchor disagree on dispatch " + op
		} else if (h.status != "" && h.status != status) || (h.repair != "" && (h.repair != repair || h.repairStatus != status)) {
			problem = "engine has not applied anchored outcome for " + op
		}
		delete(hist, op)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(hist) != 0 {
		problem = "engine is missing anchored dispatch history"
	}
	if problem != "" {
		if _, err := e.Anchor.DisableWrites(ctx, "admission recovery barrier: "+problem); err != nil {
			return anchorFault(err)
		}
		return fault("SERVICE_UNAVAILABLE", "%s; reconcile recovery before new spending", problem)
	}
	return e.requireWrites(ctx)
}
