package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
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

// requireNoRecovery refuses instruction creation and amendment while writes
// are off (NEW-RT0EE-SEC-01). After a restore the engine database may lack
// instructions the anchor shows were dispatched; an issuer change made
// before reconciliation rebuilds them would compete with the authorised
// action. Retry once the resume gate has passed (503 is retryable).
func (e *Engine) requireNoRecovery(ctx context.Context) error {
	if e.Anchor == nil {
		return nil
	}
	on, ws, err := e.writesOn(ctx)
	switch {
	case err != nil:
		return fault("SERVICE_UNAVAILABLE", "recovery anchor is unavailable; instructions cannot be created or amended")
	case !on && !ws.Enabled:
		return fault("SERVICE_UNAVAILABLE", "RECOVERY_IN_PROGRESS: writes are disabled (%s); instructions cannot be created or amended until recovery resumes writes", ws.Reason)
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
		return dependency("recovery anchor", err)
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
		dispatched, finding          bool
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
			h.dispatched = true
			if !isDispatchedMarker(out) {
				h.status = out.Status
			}
		case anchor.OperatorFinding:
			h.finding = true
		case anchor.RepairConfirmed:
			var rc RepairConfirmRecord
			if err := json.Unmarshal(r.Payload, &rc); err != nil {
				return anchorFault(err)
			}
			if rc.RepairID != "" && rc.Outcome != RepairAnchor { // an anchor repair moves no budget
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
	// An intent with no engine row is a restore unless its engine commit
	// never happened (R1-DEF-06): no DISPATCHED marker, outcome or finding,
	// and nothing at the destination.
	for op, h := range hist {
		if h.intent == nil || h.dispatched || h.finding || h.repair != "" {
			problem = "engine is missing anchored dispatch history"
			break
		}
		p, err := e.Anchor.ProofFor(ctx, h.intent.CompanyRef, op)
		if err != nil {
			return anchorFault(err)
		}
		if p.CreateIntent || p.Blocked || p.CreateDone != nil {
			problem = "engine is missing anchored dispatch history"
			break
		}
	}
	if problem != "" {
		if _, err := e.Anchor.DisableWrites(ctx, "admission recovery barrier: "+problem); err != nil {
			return anchorFault(err)
		}
		return fault("SERVICE_UNAVAILABLE", "%s; reconcile recovery before new spending", problem)
	}
	return e.requireWrites(ctx)
}

// dispatchedOutcome is the OUTCOME the engine appends right after a dispatch
// transaction commits (status PENDING, no destination result). With it, an
// intent whose engine row is missing is a restore; without it (and nothing
// at the destination) the engine commit never happened (R1-DEF-06).
const dispatchedOutcome = "DISPATCHED"

func isDispatchedMarker(o anchor.OutcomeRecord) bool {
	return o.Outcome == dispatchedOutcome && o.Status == DispatchPending
}

// markDispatched records that the dispatch of opRef committed. Best effort:
// if it is lost, a later restore of this one unsent dispatch is treated like
// an uncommitted one, which is safe (nothing was sent; the permit is still
// reserved in the restored database and can be dispatched again).
func (e *Engine) markDispatched(ctx context.Context, tenant, opRef, company string) {
	if e.Anchor == nil {
		return
	}
	if _, err := e.Anchor.Append(ctx, anchor.Outcome, tenant, opRef, anchor.OutcomeRecord{Tenant: tenant, OpRef: opRef, Destination: company,
		Outcome: dispatchedOutcome, Status: DispatchPending, Move: "reserved->unresolved"}, 0); err != nil {
		slog.Warn("recovery anchor: DISPATCHED marker not recorded", "op_ref", opRef, "err", err)
	}
}

// syncSupplier copies the destination's protected supplier revision into
// target.suppliers when the destination owns it (R1-DEF-03). The copy only
// moves forward (migration 0016). A supplier the destination does not know
// is left to the engine's own record; an unreadable destination fails the
// request, since the engine would otherwise authorise against a revision it
// cannot confirm.
func (e *Engine) syncSupplier(ctx context.Context, tenant, supplier string) error {
	src, ok := e.Connector.(connector.SupplierSource)
	if !ok || supplier == "" {
		return nil
	}
	rev, err := src.SupplierRevision(ctx, supplier)
	if err != nil {
		if strings.Contains(err.Error(), connector.ReasonMappingMissing) {
			return nil
		}
		return dependency("destination", err)
	}
	if _, err := e.Pool.Exec(ctx, `SELECT target.sync_supplier_revision($1, $2, $3)`, tenant, supplier, rev); err != nil {
		return classify(err)
	}
	return nil
}

// deliveryRevision reads the destination's protected delivery address
// revision (R1 decision 2, R1-DEF-11). The delivery check is never
// optional: with a destination connector configured, a destination that
// cannot give the revision, or has no such address, refuses (422). Only an
// R0 deployment without a connector builds actions without one.
func (e *Engine) deliveryRevision(ctx context.Context, delivery string) (*int64, error) {
	if e.Connector == nil {
		return nil, nil
	}
	src, ok := e.Connector.(connector.DeliverySource)
	if !ok {
		return nil, fault("ADAPTER_UNSUPPORTED", "the destination provides no delivery address revision; the delivery check is required")
	}
	rev, err := src.DeliveryRevision(ctx, delivery)
	if err != nil {
		if strings.Contains(err.Error(), connector.ReasonMappingMissing) {
			return nil, fault("ADAPTER_UNSUPPORTED", "delivery address %s is not mapped at the destination (%s); nothing was authorised", delivery, connector.ReasonMappingMissing)
		}
		return nil, dependency("destination", err)
	}
	if rev < 1 {
		return nil, fault("ADAPTER_UNSUPPORTED", "the destination reported no valid revision for delivery address %s", delivery)
	}
	return &rev, nil
}

// requireDeliveryRevision refuses an action without a delivery address
// revision whenever a destination connector is configured (Aakhil, 29 Oct
// 2026: the delivery check must not be optional). Such an action was built
// before the check existed or without a destination; amending it builds a
// new one with the revision.
func (e *Engine) requireDeliveryRevision(a *contract.Action) error {
	if e.Connector != nil && a.DeliveryRevision == nil {
		return fault("DELIVERY_REVISION_STALE", "the action has no delivery address revision; amend the instruction so it is authorised with one")
	}
	return nil
}

// deliveryRevisionFor reads it only for an issuer, like syncSupplierFor.
func (e *Engine) deliveryRevisionFor(ctx context.Context, p auth.Principal, delivery string) (*int64, error) {
	if p.Kind != auth.Issuer {
		return nil, nil
	}
	return e.deliveryRevision(ctx, delivery)
}

// syncSupplierFor syncs only for an issuer (the only principal that can
// create or amend an instruction), so no other caller can trigger a sync.
func (e *Engine) syncSupplierFor(ctx context.Context, p auth.Principal, supplier string) error {
	if p.Kind != auth.Issuer {
		return nil
	}
	return e.syncSupplier(ctx, p.Tenant, supplier)
}
