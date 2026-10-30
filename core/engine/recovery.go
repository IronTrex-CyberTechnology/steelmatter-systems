package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
)

// Restore detection, reconciliation and the resume gate (D5 v3, decided
// 28 Oct 2026). The anchor is outside both the engine database's and Odoo's
// rollback domain, so comparing the three shows what a restore erased.
// Writes go off on any signal and come back only through TryResume.

// Restore signal codes.
const (
	SignalEngineMissingOperation = "ENGINE_MISSING_OPERATION"      // anchor has a dispatch intent the engine lacks (engine restored)
	SignalAnchorMissingOperation = "ANCHOR_MISSING_OPERATION"      // engine has an operation the anchor lacks (anchor behind)
	SignalEngineStatusBehind     = "ENGINE_STATUS_BEHIND"          // anchor recorded a final status the engine lacks (engine restored)
	SignalDestinationBehind      = "DESTINATION_BEHIND"            // destination head is behind what the anchor has seen (Odoo restored)
	SignalUnknownDestinationOp   = "UNKNOWN_DESTINATION_OPERATION" // destination has an op_ref neither anchor nor engine knows (case C)
	SignalCommittedOrderMissing  = "COMMITTED_ORDER_MISSING"       // committed operation whose destination order is gone (case E)
	SignalClockSkew              = "CLOCK_SKEW"                    // engine and anchor clocks differ by more than the limit, or cannot be compared
	FlagOrderMissingAfterRestore = "ORDER_MISSING_AFTER_RESTORE"
	// FlagOrderDeletedAtDestination marks a committed operation whose order
	// was deleted at the destination; the journal records the deletion, so
	// it is not a restore and writes stay on (R1-DEF-20).
	FlagOrderDeletedAtDestination = "ORDER_DELETED_AT_DESTINATION"
	// FlagOrderChangedAtDestination marks a committed operation whose order
	// was edited or cancelled at the destination after it was committed: the
	// journal records po_edited or po_cancelled, or the observer's readback
	// no longer matches. Not a restore; writes stay on; no refund (R1-DEF-13).
	FlagOrderChangedAtDestination = "ORDER_CHANGED_AT_DESTINATION"
	systemTenant                  = "_system"
	writesDisabledEvent           = "system.writes_disabled"
	writesEnabledEvent            = "system.writes_enabled"
	recoveredEvent                = "operation.recovered"
	reasonRestoreDetected         = "RESTORE_DETECTED"
	reasonResumeGatePassed        = "RESUME_GATE_PASSED"
	destinationListPage           = 500
)

// RestoreSignal is one sign that a store was restored.
type RestoreSignal struct {
	Code   string `json:"code"`
	Tenant string `json:"tenant_id,omitempty"`
	OpRef  string `json:"op_ref,omitempty"`
	Detail string `json:"detail"`
}

// RestoreReport is the result of one restore check.
type RestoreReport struct {
	Signals        []RestoreSignal   `json:"signals"`
	WritesDisabled bool              `json:"writes_disabled"` // this check turned writes off
	WriteState     anchor.WriteState `json:"write_state"`
	// Flagged lists committed operations this check flagged because their
	// order was deleted at the destination (R1-DEF-20). They stay committed;
	// writes stay on for everything else.
	Flagged []string `json:"flagged"`
	// Unconfirmed lists dispatch intents whose engine commit never happened
	// (R1-DEF-06): not a restore; the client may retry the same dispatch.
	Unconfirmed []string `json:"unconfirmed"`
}

var finalStatus = map[string]bool{DispatchCommitted: true, DispatchFenced: true, DispatchMismatch: true, DispatchHeld: true}

type engineOp struct {
	tenant, opRef, status, flag, mandate string
	amount                               int64
}

type dispKey = [2]string

// recoveryView is one consistent read of the anchor, the engine database
// and the destination journal.
type recoveryView struct {
	intents  map[dispKey]anchor.IntentRecord
	outcomes map[dispKey]anchor.OutcomeRecord
	young    map[dispKey]bool // anchor records newer than RestoreGrace
	youngOps map[dispKey]bool // engine operations changed within RestoreGrace
	findings map[dispKey]bool
	// dispatched: the engine committed this dispatch (the DISPATCHED marker
	// written after commit, or any later outcome). An intent without it, with
	// no engine row and nothing at the destination, is a dispatch whose
	// engine commit never happened (R1-DEF-06), not a restore.
	dispatched map[dispKey]bool
	proofs     map[string]connector.Proof
	ops        map[dispKey]engineOp
	dest       map[string]connector.ListRow // latest journal row per op_ref
	head       int64
	minSeq     int64
}

func (e *Engine) recoveryReady(c connector.Connector) error {
	if e.Anchor == nil {
		return fault("SERVICE_UNAVAILABLE", "no recovery anchor is configured")
	}
	if c == nil {
		return fault("SERVICE_UNAVAILABLE", "no destination connector is configured")
	}
	return nil
}

// readView reads the three stores in the one order that cannot produce a
// false alarm under concurrent traffic: the destination first, then the
// engine database, then the anchor. An order at the destination was
// dispatched (engine committed) before it was created, and every engine
// commit was preceded by its anchor append, so each later read already
// holds what the earlier one saw. Records newer than RestoreGrace are left
// out of the comparisons that look the other way.
func (e *Engine) readView(ctx context.Context, c connector.Connector) (*recoveryView, error) {
	return e.readViewWithDB(ctx, c, e.Pool)
}

type recoveryQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// A caller already holding an engine transaction must reuse its connection.
// Acquiring another pool connection while peers wait on its mandate lock can
// exhaust the pool and prevent the lock holder from making progress.
func (e *Engine) readViewWithDB(ctx context.Context, c connector.Connector, db recoveryQuerier) (*recoveryView, error) {
	v := &recoveryView{intents: map[dispKey]anchor.IntentRecord{}, outcomes: map[dispKey]anchor.OutcomeRecord{}, young: map[dispKey]bool{},
		youngOps: map[dispKey]bool{}, findings: map[dispKey]bool{}, dispatched: map[dispKey]bool{}, ops: map[dispKey]engineOp{}, dest: map[string]connector.ListRow{}}
	var cp int64
	for {
		page, err := c.List(ctx, cp, destinationListPage)
		if err != nil {
			return nil, dependency("destination", err)
		}
		v.head = page.HeadSeq
		for _, r := range page.Rows {
			if !r.Foreign && r.OpRef != "" {
				v.dest[r.OpRef] = r
			}
		}
		if len(page.Rows) == 0 || page.NextCheckpoint <= cp {
			break
		}
		cp = page.NextCheckpoint
	}

	rows, err := db.Query(ctx, `SELECT tenant_id, op_ref, status, COALESCE(flag, ''), mandate_id, amount_minor,
		updated_at > clock_timestamp() - make_interval(secs => $1) FROM steelmatter.dispatches`, e.Cfg.RestoreGrace.Seconds())
	if err != nil {
		return nil, unavailable(err)
	}
	for rows.Next() {
		var o engineOp
		var young bool
		if err := rows.Scan(&o.tenant, &o.opRef, &o.status, &o.flag, &o.mandate, &o.amount, &young); err != nil {
			rows.Close()
			return nil, err
		}
		k := dispKey{o.tenant, o.opRef}
		v.ops[k] = o
		v.youngOps[k] = young
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}

	now, err := e.Anchor.Now(ctx)
	if err != nil {
		return nil, dependency("recovery anchor", err)
	}
	cutoff := now.Add(-e.Cfg.RestoreGrace)
	for _, typ := range []string{anchor.DispatchIntent, anchor.Outcome, anchor.OperatorFinding} {
		recs, err := e.Anchor.Records(ctx, typ)
		if err != nil {
			return nil, dependency("recovery anchor", err)
		}
		for _, r := range recs {
			k := dispKey{r.Tenant, r.OpRef}
			if r.RecordedAt.After(cutoff) && typ != anchor.OperatorFinding {
				v.young[k] = true
			}
			switch typ {
			case anchor.DispatchIntent:
				var ir anchor.IntentRecord
				if err := json.Unmarshal(r.Payload, &ir); err != nil {
					return nil, fmt.Errorf("anchor record %d: %w", r.Seq, err)
				}
				v.intents[k] = ir
			case anchor.Outcome:
				var o anchor.OutcomeRecord
				if err := json.Unmarshal(r.Payload, &o); err != nil {
					return nil, fmt.Errorf("anchor record %d: %w", r.Seq, err)
				}
				v.dispatched[k] = true
				if isDispatchedMarker(o) {
					continue // not a destination outcome
				}
				v.outcomes[k] = o
			case anchor.OperatorFinding:
				v.findings[k] = true
			}
		}
	}
	if v.proofs, err = e.Anchor.Proofs(ctx, e.Cfg.CompanyRef); err != nil {
		return nil, dependency("recovery anchor", err)
	}
	if v.minSeq, err = e.Anchor.ExpectedMinSeqBefore(ctx, e.Cfg.CompanyRef, cutoff); err != nil {
		return nil, dependency("recovery anchor", err)
	}
	return v, nil
}

func (v *recoveryView) knownOp(op string) bool {
	for k := range v.ops {
		if k[1] == op {
			return true
		}
	}
	for k := range v.intents {
		if k[1] == op {
			return true
		}
	}
	return false
}

// unconfirmed reports whether k is a dispatch intent whose engine commit
// never happened (R1-DEF-06): no engine row, no DISPATCHED marker or
// outcome, no finding, and nothing at the destination (no CREATE_INTENT,
// BLOCK or CREATE_DONE, no journal row). Nothing was consumed in the engine
// and nothing was sent, so it is not a restore: the client can retry the
// same dispatch, and the permit expires normally if it does not. A restore
// that rolled back a committed dispatch is still detected through the
// DISPATCHED marker the engine writes after every commit.
func (v *recoveryView) unconfirmed(k dispKey) bool {
	if _, ok := v.intents[k]; !ok {
		return false
	}
	if _, ok := v.ops[k]; ok {
		return false
	}
	p := v.proofs[k[1]]
	_, inDest := v.dest[k[1]]
	return !v.dispatched[k] && !v.findings[k] && !p.CreateIntent && !p.Blocked && p.CreateDone == nil && !inDest
}

func (v *recoveryView) unconfirmedOps() []string {
	out := []string{}
	for k := range v.intents {
		if v.unconfirmed(k) {
			out = append(out, k[1])
		}
	}
	sort.Strings(out)
	return out
}

// flagDeletedOrders flags every committed operation whose order the
// destination journal shows deleted (R1-DEF-20). The operation stays
// committed (no refund, no bucket move) and the op_ref can never be created
// again (the destination keeps its operation row). Only this operation is
// affected; a HELD finding can be recorded against it.
func (e *Engine) flagDeletedOrders(ctx context.Context, v *recoveryView) ([]string, error) {
	out := []string{}
	keys := make([]dispKey, 0)
	flags := map[dispKey]string{}
	for k, o := range v.ops {
		if o.status != DispatchCommitted {
			continue
		}
		switch {
		case v.deletedAtDestination(k[1]) && (o.flag == "" || o.flag == FlagOrderChangedAtDestination):
			flags[k] = FlagOrderDeletedAtDestination
		case v.changedAtDestination(k[1]) && o.flag == "":
			flags[k] = FlagOrderChangedAtDestination // R1-DEF-13
		default:
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][1] < keys[j][1] })
	for _, k := range keys {
		outcome := "ORDER_DELETED"
		if flags[k] == FlagOrderChangedAtDestination {
			outcome = "ORDER_CHANGED"
		}
		r := connector.Result{OpRef: k[1], Outcome: connector.Outcome(outcome), Seq: v.dest[k[1]].Seq}
		flagged, err := e.flagCommittedAs(ctx, v.ops[k], r, flags[k])
		if err != nil {
			return out, err
		}
		if flagged {
			out = append(out, k[1])
		}
	}
	return out, nil
}

// deletedAtDestination reports whether the destination journal's latest
// row for op records that its order was deleted there (not a restore).
func (v *recoveryView) deletedAtDestination(op string) bool {
	return v.dest[op].Event == "po_deleted"
}

// changedAtDestination reports whether the destination journal's latest row
// for op records an edit or a cancellation of its order (R1-DEF-13).
func (v *recoveryView) changedAtDestination(op string) bool {
	ev := v.dest[op].Event
	return ev == "po_edited" || ev == "po_cancelled"
}

// orderPresent reports whether the destination journal holds a created
// order for op.
func (v *recoveryView) orderPresent(op string) bool {
	r, ok := v.dest[op]
	return ok && r.State == "created" && r.OrderID != ""
}

func (v *recoveryView) signals() []RestoreSignal {
	var out []RestoreSignal
	for k, ir := range v.intents {
		if _, ok := v.ops[k]; !ok && !v.young[k] && !v.unconfirmed(k) {
			out = append(out, RestoreSignal{SignalEngineMissingOperation, k[0], k[1],
				fmt.Sprintf("anchor holds a dispatch intent for %d minor units that the engine database lacks", ir.AmountMinor)})
		}
	}
	for k, o := range v.ops {
		if _, ok := v.intents[k]; !ok {
			out = append(out, RestoreSignal{SignalAnchorMissingOperation, k[0], k[1], "engine database holds an operation the anchor lacks"})
		}
		if a, ok := v.outcomes[k]; ok && finalStatus[a.Status] && a.Status != o.status && !v.young[k] {
			out = append(out, RestoreSignal{SignalEngineStatusBehind, k[0], k[1],
				fmt.Sprintf("anchor recorded %s, engine database has %s", a.Status, o.status)})
		}
		if o.status == DispatchCommitted && o.flag == "" && !v.orderPresent(k[1]) && !v.youngOps[k] && !v.deletedAtDestination(k[1]) {
			out = append(out, RestoreSignal{SignalCommittedOrderMissing, k[0], k[1], "committed operation has no order at the destination"})
		}
	}
	if v.head < v.minSeq {
		out = append(out, RestoreSignal{Code: SignalDestinationBehind,
			Detail: fmt.Sprintf("destination head %d is behind sequence %d already seen", v.head, v.minSeq)})
	}
	for op := range v.dest {
		if !v.knownOp(op) {
			out = append(out, RestoreSignal{Code: SignalUnknownDestinationOp, OpRef: op, Detail: "destination holds an operation neither the anchor nor the engine knows"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].OpRef < out[j].OpRef
	})
	return out
}

// CheckRestore compares the anchor with the engine database and the
// destination journal (mechanism step 6). Any signal turns writes off and
// bumps the generation, so no worker from before the restore can dispatch.
// A failed comparison disables new writes as well; an unreadable store is
// not evidence that recovery is safe. Held amounts remain held.
func (e *Engine) CheckRestore(ctx context.Context, c connector.Connector) (*RestoreReport, error) {
	if err := e.recoveryReady(c); err != nil {
		return nil, err
	}
	v, err := e.readView(ctx, c)
	if err != nil {
		_, disableErr := e.Anchor.DisableWrites(ctx, "restore comparison unavailable")
		return nil, errors.Join(err, disableErr)
	}
	rep := &RestoreReport{Signals: v.signals(), Flagged: []string{}, Unconfirmed: v.unconfirmedOps()}
	if skew, cerr := e.anchorClockSkew(ctx); cerr != nil || skew > e.Cfg.MaxClockSkew {
		detail := fmt.Sprintf("engine and recovery anchor clocks differ by %s (limit %s); timestamps cannot be compared safely", skew.Round(time.Millisecond), e.Cfg.MaxClockSkew)
		if cerr != nil {
			detail = "the recovery anchor's clock cannot be read: " + cerr.Error()
		}
		rep.Signals = append(rep.Signals, RestoreSignal{Code: SignalClockSkew, Detail: detail})
	}
	if rep.Flagged, err = e.flagDeletedOrders(ctx, v); err != nil {
		return nil, err
	}
	if rep.WriteState, err = e.Anchor.WriteState(ctx); err != nil {
		return nil, dependency("recovery anchor", err)
	}
	if len(rep.Signals) == 0 || !rep.WriteState.Enabled {
		return rep, nil
	}
	codes := map[string]int{}
	for _, s := range rep.Signals {
		codes[s.Code]++
	}
	var parts []string
	for c, n := range codes {
		parts = append(parts, fmt.Sprintf("%s x%d", c, n))
	}
	sort.Strings(parts)
	reason := reasonRestoreDetected + ": " + strings.Join(parts, ", ")
	if rep.WriteState, err = e.Anchor.DisableWrites(ctx, reason); err != nil {
		return nil, dependency("recovery anchor", err)
	}
	rep.WritesDisabled = true
	if err := e.systemEvent(ctx, writesDisabledEvent, rep.WriteState.Generation, map[string]any{"reason": reason, "signals": rep.Signals,
		"generation": rep.WriteState.Generation}); err != nil {
		return rep, err
	}
	return rep, nil
}

func (e *Engine) systemEvent(ctx context.Context, typ string, gen int64, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := e.Pool.Exec(ctx, `INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp()) ON CONFLICT ON CONSTRAINT outbox_once DO NOTHING`,
		newID("evt"), systemTenant, typ, fmt.Sprintf("writes-generation-%d", gen), b)
	return classify(err)
}

// ReconcileReport is what one reconciliation pass did.
type ReconcileReport struct {
	Recreated []string `json:"recreated"` // op_refs rebuilt from the anchor as UNRESOLVED (cases A, B, G)
	FromOdoo  []string `json:"from_odoo"` // op_refs only Odoo knew, rebuilt from the engine's own permit (case C)
	Flagged   []string `json:"flagged"`   // committed operations with the order gone; BLOCK written, still committed (case E)
	Refenced  []string `json:"refenced"`  // fenced operations re-fenced after a destination restore
	Settled   []string `json:"settled"`   // "op_ref=STATUS" for operations this pass brought to a final state
	Problems  []string `json:"problems"`  // anything that needs an operator; the gate will not pass
	// Unconfirmed lists dispatch intents whose engine commit never happened
	// (R1-DEF-06); they are left for the client to retry, never rebuilt.
	Unconfirmed []string `json:"unconfirmed"`
	// Complete is false when the pass stopped at its time budget before
	// finishing (R1-DEF-30); everything it did is consistent, and running
	// reconciliation again continues from there.
	Complete bool `json:"complete"`
}

// ReconcileRestore accounts for every operation a restore may have erased (cases
// A to G). It runs only while writes are off: it rebuilds missing
// operations from the anchor as UNRESOLVED with their amount held, rebuilds
// an order only Odoo knows from the engine's own permit (case C), writes a
// BLOCK for committed operations whose order is gone (they stay committed),
// re-fences fenced operations, and settles everything unsettled through
// lookup and fence. It never creates an order and never releases an amount
// without proof. Running it again is safe.
func (e *Engine) ReconcileRestore(ctx context.Context, c connector.Connector) (*ReconcileReport, error) {
	if err := e.recoveryReady(c); err != nil {
		return nil, err
	}
	unlock, err := e.recoveryLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return nil, dependency("recovery anchor", err)
	}
	if ws.Enabled {
		return nil, fault("FORBIDDEN", "reconciliation runs only while writes are off")
	}
	v, err := e.readView(ctx, c)
	if err != nil {
		return nil, err
	}
	rep := &ReconcileReport{Unconfirmed: v.unconfirmedOps()}
	budget := newBudget(ctx)
	keys := make([]dispKey, 0, len(v.intents))
	for k := range v.intents {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][1] < keys[j][1] })
	for _, k := range keys {
		if _, ok := v.ops[k]; ok || v.unconfirmed(k) {
			continue
		}
		if budget.spent() {
			return rep, nil
		}
		created, err := e.recreateDispatch(ctx, v.intents[k])
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: cannot rebuild from the anchor: %s", k[1], problemText(err)))
			continue
		}
		if created {
			rep.Recreated = append(rep.Recreated, k[1])
		}
	}
	// Case C: an order Odoo has that neither the anchor nor the engine knows.
	ops := make([]string, 0, len(v.dest))
	for op := range v.dest {
		if !v.knownOp(op) {
			ops = append(ops, op)
		}
	}
	sort.Strings(ops)
	for _, op := range ops {
		if budget.spent() {
			return rep, nil
		}
		created, err := e.recreateFromDestination(ctx, v.dest[op])
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: order known only to Odoo; cannot rebuild: %s", op, problemText(err)))
			continue
		}
		if created {
			rep.FromOdoo = append(rep.FromOdoo, op)
		}
	}
	// Settle everything unsettled; writes are off, so nothing is executed.
	for round := 0; round < 3; round++ {
		rows, err := e.Pool.Query(ctx, `SELECT tenant_id, op_ref FROM steelmatter.dispatches WHERE status IN ('PENDING', 'UNRESOLVED') ORDER BY op_ref`)
		if err != nil {
			return rep, unavailable(err)
		}
		var todo []dispKey
		for rows.Next() {
			var k dispKey
			if err := rows.Scan(&k[0], &k[1]); err != nil {
				rows.Close()
				return rep, err
			}
			todo = append(todo, k)
		}
		rows.Close()
		if len(todo) == 0 {
			break
		}
		for _, k := range todo {
			if budget.spent() {
				return rep, nil
			}
			r, changed, err := e.settle(ctx, k[0], k[1], c)
			if err != nil {
				if round == 2 {
					rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %s", k[1], problemText(err)))
				}
				continue
			}
			if finalStatus[r.Execution] {
				if changed {
					rep.Settled = append(rep.Settled, k[1]+"="+r.Execution)
				}
			} else if round == 2 {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: still %s after %s", k[1], r.Execution, r.LastOutcome))
			}
		}
	}
	problems, err := e.replayConfirmedRepairs(ctx)
	if err != nil {
		return rep, err
	}
	rep.Problems = append(rep.Problems, problems...)
	// Then, on a fresh read, block committed operations whose order is gone
	// and re-fence fenced ones. This runs after settling so that an
	// operation committed just now from a surviving CREATE_DONE (engine and
	// Odoo both restored) is blocked and flagged in the same pass.
	if v, err = e.readView(ctx, c); err != nil {
		return rep, err
	}
	deleted, err := e.flagDeletedOrders(ctx, v)
	if err != nil {
		return rep, err
	}
	rep.Flagged = append(rep.Flagged, deleted...)
	for k, o := range v.ops {
		switch {
		case o.status == DispatchCommitted && o.flag == "" && v.deletedAtDestination(k[1]):
			// Flagged just above; deletion at the destination is not a restore.
		case e.Cfg.ObserveOnly && ((o.status == DispatchCommitted && o.flag == "" && !v.orderPresent(k[1])) ||
			(o.status == DispatchFenced && v.proofs[k[1]].Blocked && v.dest[k[1]].State == "")):
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: needs a fence at the destination; OBSERVE_ONLY never writes there, "+
				"so a deployment with write authority must reconcile it", k[1]))
		case o.status == DispatchCommitted && o.flag == "" && !v.orderPresent(k[1]):
			r, err := c.Fence(ctx, k[1], 0)
			if err != nil || r.BlockCommitted == nil || !*r.BlockCommitted {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: committed order missing; BLOCK not yet committed (%s %v)", k[1], r.Outcome, err))
				continue
			}
			if err := e.flagCommitted(ctx, o, r); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", k[1], err))
				continue
			}
			rep.Flagged = append(rep.Flagged, k[1])
		case o.status == DispatchFenced && !v.proofs[k[1]].Blocked:
			// A fence the destination's restore-proof record does not show.
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: fenced but the destination has no BLOCK record", k[1]))
		case o.status == DispatchFenced && v.dest[k[1]].State == "":
			r, err := c.Fence(ctx, k[1], 0)
			if err != nil || !r.ProvesNoOrder() {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: re-fence after a destination restore did not prove no order (%s %v)", k[1], r.Outcome, err))
				continue
			}
			rep.Refenced = append(rep.Refenced, k[1])
		}
	}
	rep.Complete = true
	return rep, nil
}

// recreateDispatch rebuilds an operation the engine database lost from its
// anchor DISPATCH_INTENT, as UNRESOLVED with its amount held: from the
// reservation if the permit is still reserved, otherwise from available.
// It refuses rather than over-commit the mandate.
func (e *Engine) recreateDispatch(ctx context.Context, ir anchor.IntentRecord) (bool, error) {
	created := false
	err := e.inTx(ctx, ir.OpRef, func(tx pgx.Tx) error {
		created = false
		m, err := lockMandate(ctx, tx, ir.Tenant, ir.MandateID, "FOR UPDATE")
		if err != nil {
			if fe, ok := err.(*Fault); ok && fe.Code == "NOT_FOUND" {
				return fault("NOT_FOUND", "MANDATE_MISSING: mandate %s of operation %s is not in the engine database (it was restored to before the mandate existed); "+
					"restore or re-provision the mandate, then reconcile again", ir.MandateID, ir.OpRef)
			}
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2)`,
			ir.Tenant, ir.OpRef).Scan(&exists); err != nil || exists {
			return err
		}
		var status string
		var amount int64
		err = tx.QueryRow(ctx, `SELECT status, amount_minor FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
			ir.Tenant, ir.PermitID).Scan(&status, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			// R1-DEF-05: the engine was restored to before the permit (and
			// possibly the instruction) existed. Rebuild them from the
			// anchor's signed intent and hold the amount again.
			if err := e.rebuildFromIntent(ctx, tx, ir, m); err != nil {
				return err
			}
			status, amount = "ACTIVE", ir.AmountMinor
			err = nil
		}
		if err != nil {
			return err
		}
		if amount != ir.AmountMinor {
			return fault("REVISION_STALE", "PERMIT_AMOUNT_DIFFERS: permit %s holds %d minor units, the anchor's intent %d", ir.PermitID, amount, ir.AmountMinor)
		}
		from := "reserved_minor"
		switch status {
		case "UNSIGNED":
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET permit_jws = $3, signed_at = clock_timestamp(), status = 'ACTIVE'
				WHERE tenant_id = $1 AND permit_id = $2`, ir.Tenant, ir.PermitID, ir.PermitJWS); err != nil {
				return err
			}
			fallthrough
		case "ACTIVE":
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'CONSUMED', consumed_at = clock_timestamp()
				WHERE tenant_id = $1 AND permit_id = $2`, ir.Tenant, ir.PermitID); err != nil {
				return err
			}
		case "RELEASED":
			from = "available_minor"
		default:
			return fault("REVISION_STALE", "PERMIT_USED_ELSEWHERE: permit %s is %s by another operation", ir.PermitID, status)
		}
		var avail int64
		if err := tx.QueryRow(ctx, `SELECT `+from+` FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`, ir.Tenant, ir.MandateID).Scan(&avail); err != nil {
			return err
		}
		if avail < ir.AmountMinor {
			return fault("REVISION_STALE", "INSUFFICIENT_BUDGET: mandate %s has only %d minor units in %s to hold %d again", ir.MandateID, avail, from, ir.AmountMinor)
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp())`).Scan(&now); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"op_ref": ir.OpRef, "amount_minor": ir.AmountMinor, "source": "anchor"})
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO steelmatter.dispatches (tenant_id, op_ref, instruction_id, action_revision, mandate_id, company_ref, permit_id, control_epoch,
			  action_canonical, action_digest, permit_digest, amount_minor, status, created_at, updated_at, recovered_at, admitted_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'UNRESOLVED', $13, $13, $13, NULLIF($14, '')::timestamptz)`,
				[]any{ir.Tenant, ir.OpRef, ir.InstructionID, ir.ActionRevision, ir.MandateID, ir.CompanyRef, ir.PermitID, ir.ControlEpoch,
					[]byte(ir.ActionCanonical), ir.ActionDigest, ir.PermitDigest, ir.AmountMinor, now, ir.AdmittedAt}},
			{`UPDATE steelmatter.action_revisions SET status = 'COMMITTED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
				[]any{ir.Tenant, ir.InstructionID, ir.ActionRevision}},
			{`UPDATE steelmatter.mandates SET ` + from + ` = ` + from + ` - $3, unresolved_minor = unresolved_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`,
				[]any{ir.Tenant, ir.MandateID, ir.AmountMinor}},
			{`INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				[]any{newID("evt"), ir.Tenant, recoveredEvent, ir.OpRef, payload, now}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return created && err == nil, err
}

// recreateFromDestination rebuilds an operation that only Odoo knows (case
// C) from the engine's own record of its permit: op_ref is the operation ID
// the engine assigned when it issued the permit, so the issuance, the
// action revision it authorised and the permit digest are still in the
// engine database. The order must carry that permit digest and amount;
// otherwise nothing is rebuilt and an operator takes over. The rebuilt
// operation is UNRESOLVED with its amount held; settling then verifies the
// order (COMMITTED if it matches, MISMATCH if not). Its missing anchor
// intent keeps the resume gate closed until Aakhil confirms the anchor
// repair (ConfirmAnchorRepair).
func (e *Engine) recreateFromDestination(ctx context.Context, row connector.ListRow) (bool, error) {
	rows, err := e.Pool.Query(ctx, `SELECT i.tenant_id, i.permit_id, i.mandate_id, i.instruction_id, i.action_revision, i.control_epoch,
		i.amount_minor, i.permit_digest, COALESCE(i.permit_jws, ''), a.action_canonical, a.action_digest
		FROM steelmatter.issuances i
		JOIN steelmatter.action_revisions a ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.action_revision
		WHERE i.operation_id = $1`, row.OpRef)
	if err != nil {
		return false, unavailable(err)
	}
	var found []anchor.IntentRecord
	for rows.Next() {
		var ir anchor.IntentRecord
		var canonical []byte
		if err := rows.Scan(&ir.Tenant, &ir.PermitID, &ir.MandateID, &ir.InstructionID, &ir.ActionRevision, &ir.ControlEpoch,
			&ir.AmountMinor, &ir.PermitDigest, &ir.PermitJWS, &canonical, &ir.ActionDigest); err != nil {
			rows.Close()
			return false, err
		}
		ir.OpRef, ir.CompanyRef, ir.ActionCanonical = row.OpRef, e.Cfg.CompanyRef, string(canonical)
		found = append(found, ir)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, classify(err)
	}
	switch {
	case len(found) == 0:
		return false, errors.New("the engine database has no permit for this operation")
	case len(found) > 1:
		return false, errors.New("more than one permit claims this operation")
	}
	ir := found[0]
	if row.PermitDigest != ir.PermitDigest {
		return false, errors.New("the order's permit digest does not match the engine's permit")
	}
	if row.AmountMinor != ir.AmountMinor {
		return false, fmt.Errorf("the order's amount %d does not match the permit's %d", row.AmountMinor, ir.AmountMinor)
	}
	if ir.PermitJWS == "" {
		return false, errors.New("the engine's permit was never signed, so no order can be attributed to it")
	}
	return e.recreateDispatch(ctx, ir)
}

// flagCommitted marks a committed operation whose order is gone (case E).
// It stays committed: no refund, no bucket move.
func (e *Engine) flagCommitted(ctx context.Context, o engineOp, r connector.Result) error {
	_, err := e.flagCommittedAs(ctx, o, r, FlagOrderMissingAfterRestore)
	return err
}

// flagCommittedAs sets flag on a committed operation once and records it in
// the anchor. It reports whether this call set it.
func (e *Engine) flagCommittedAs(ctx context.Context, o engineOp, r connector.Result, flag string) (bool, error) {
	set := false
	err := e.inTx(ctx, o.opRef, func(tx pgx.Tx) error {
		set = false
		if _, err := lockMandate(ctx, tx, o.tenant, o.mandate, "FOR UPDATE"); err != nil {
			return err
		}
		// A flag is set once; only a deletion may replace a change flag.
		tag, err := tx.Exec(ctx, `UPDATE steelmatter.dispatches SET flag = $3, updated_at = clock_timestamp()
			WHERE tenant_id = $1 AND op_ref = $2 AND status = 'COMMITTED'
			AND (flag IS NULL OR (flag = 'ORDER_CHANGED_AT_DESTINATION' AND $3 = 'ORDER_DELETED_AT_DESTINATION'))`, o.tenant, o.opRef, flag)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		set = true
		return e.anchorOutcome(ctx, anchor.OutcomeRecord{Tenant: o.tenant, OpRef: o.opRef, Destination: e.Cfg.CompanyRef, Outcome: string(r.Outcome),
			Status: DispatchCommitted, Move: "none", DestinationSeq: r.Seq, BlockCommitted: r.BlockCommitted, PriorCreate: r.PriorCreate, CreateDone: r.CreateDone})
	})
	return set && err == nil, err
}

// GateCheck is one resume-gate condition.
type GateCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// GateReport is the result of TryResume.
type GateReport struct {
	Checks     []GateCheck       `json:"checks"`
	Passed     bool              `json:"passed"`
	Resumed    bool              `json:"resumed"`
	WriteState anchor.WriteState `json:"write_state"`
}

// TryResume runs the resume gate. Writes restart only when every condition
// holds; an operator can run reconciliation again but cannot skip a check.
// HELD and MISMATCH operations follow the D5 HELD rule: they do not keep
// writes off if their op_ref is permanently blocked, their amount is held
// and a HELD finding is recorded; they stay held after writes resume.
func (e *Engine) TryResume(ctx context.Context, c connector.Connector) (*GateReport, error) {
	if err := e.recoveryReady(c); err != nil {
		return nil, err
	}
	unlock, err := e.recoveryLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return nil, dependency("recovery anchor", err)
	}
	rep := &GateReport{WriteState: ws}
	if ws.Enabled {
		rep.Passed = true
		rep.Checks = []GateCheck{{Name: "WRITES_ALREADY_ON", Passed: true, Detail: fmt.Sprintf("generation %d", ws.Generation)}}
		return rep, nil
	}
	v, err := e.readView(ctx, c)
	add := func(name string, problems []string) {
		gc := GateCheck{Name: name, Passed: len(problems) == 0}
		if len(problems) > 0 {
			sort.Strings(problems)
			gc.Detail = strings.Join(problems, "; ")
		}
		rep.Checks = append(rep.Checks, gc)
	}
	if err != nil {
		add("ALL_READABLE", []string{err.Error()})
		return rep, nil
	}
	add("ALL_READABLE", nil)
	if skew, cerr := e.anchorClockSkew(ctx); cerr != nil {
		add("CLOCKS_AGREE", []string{"the recovery anchor's clock cannot be read: " + cerr.Error()})
	} else if skew > e.Cfg.MaxClockSkew {
		add("CLOCKS_AGREE", []string{fmt.Sprintf("engine and recovery anchor clocks differ by %s (limit %s)", skew.Round(time.Millisecond), e.Cfg.MaxClockSkew)})
	} else {
		add("CLOCKS_AGREE", nil)
	}

	var missing, unsettled, held, anchorBehind []string
	for k := range v.intents {
		if _, ok := v.ops[k]; !ok && !v.unconfirmed(k) {
			missing = append(missing, k[1]+" (anchor)")
		}
	}
	for op := range v.dest {
		if !v.knownOp(op) {
			missing = append(missing, op+" (destination)")
		}
	}
	add("ALL_OPERATIONS_PRESENT", missing)

	for k, o := range v.ops {
		switch {
		case o.status == DispatchPending || o.status == DispatchUnresolved:
			unsettled = append(unsettled, k[1]+" "+o.status)
		case o.status == DispatchHeld:
			if !v.proofs[k[1]].Blocked {
				held = append(held, k[1]+": HELD but its op_ref is not blocked in the anchor")
			}
			if !v.findings[k] {
				held = append(held, k[1]+": HELD without a recorded HELD finding")
			}
		case o.status == DispatchMismatch:
			if !v.orderPresent(k[1]) && !v.proofs[k[1]].Blocked {
				held = append(held, k[1]+": MISMATCH with no order at the destination and no BLOCK")
			}
			if !v.findings[k] {
				held = append(held, k[1]+": MISMATCH without a recorded finding")
			}
		case o.flag == FlagOrderDeletedAtDestination:
			// Deleted at the destination, not a restore: the destination keeps
			// its operation row, so the op_ref can never be created again.
			if !v.findings[k] {
				held = append(held, k[1]+": order deleted at the destination without a recorded finding")
			}
		case o.flag == FlagOrderChangedAtDestination:
			// Edited or cancelled at the destination (R1-DEF-13): stays
			// committed; an operator records what happened.
			if !v.findings[k] {
				held = append(held, k[1]+": order changed at the destination without a recorded finding")
			}
		case o.flag != "":
			if !v.proofs[k[1]].Blocked {
				held = append(held, k[1]+": committed order missing and op_ref not blocked")
			}
			if !v.findings[k] {
				held = append(held, k[1]+": committed order missing without a recorded finding")
			}
		}
		if _, ok := v.intents[k]; !ok {
			anchorBehind = append(anchorBehind, k[1]+": the anchor lacks this operation; Aakhil must confirm the anchor repair")
		}
	}
	add("NONE_PENDING_OR_UNRESOLVED", unsettled)
	add("HELD_RULE", held)
	buckets, err := e.bucketProblems(ctx)
	if err != nil {
		add("BUCKETS_RECONCILE", []string{err.Error()})
	} else {
		add("BUCKETS_RECONCILE", buckets)
	}
	add("ANCHOR_NOT_BEHIND", anchorBehind)
	var other []string
	for _, s := range v.signals() {
		switch s.Code {
		case SignalDestinationBehind: // re-baselined when writes resume
		case SignalEngineMissingOperation, SignalAnchorMissingOperation, SignalUnknownDestinationOp:
			// covered by the checks above (a committed operation whose order is
			// missing is NOT: until reconciliation blocks and flags it, it fails here)
		default:
			other = append(other, s.Code+" "+s.OpRef+": "+s.Detail)
		}
	}
	add("NO_UNEXPLAINED_SIGNALS", other)

	rep.Passed = true
	for _, gc := range rep.Checks {
		rep.Passed = rep.Passed && gc.Passed
	}
	if !rep.Passed {
		return rep, nil
	}
	if rep.WriteState, err = e.Anchor.EnableWrites(ctx, ws.Generation, reasonResumeGatePassed, v.head); err != nil {
		return rep, dependency("recovery anchor", err)
	}
	e.anchorGen.Store(rep.WriteState.Generation)
	rep.Resumed = true
	return rep, e.systemEvent(ctx, writesEnabledEvent, rep.WriteState.Generation, map[string]any{"generation": rep.WriteState.Generation,
		"baseline_seq": v.head})
}

// bucketProblems rebuilds every mandate's buckets from its records and
// reports any that differ from the stored buckets: committed = consumed
// permits without a dispatch (R0) plus COMMITTED dispatches; unresolved =
// PENDING, UNRESOLVED, HELD and MISMATCH dispatches; reserved = unsigned and
// active permits; available = the rest of the limit, never negative.
func (e *Engine) bucketProblems(ctx context.Context) ([]string, error) {
	rows, err := e.Pool.Query(ctx, `
		SELECT m.tenant_id, m.mandate_id, m.limit_minor, m.available_minor, m.reserved_minor, m.committed_minor, m.unresolved_minor,
			COALESCE((SELECT sum(i.amount_minor) FROM steelmatter.issuances i WHERE i.tenant_id = m.tenant_id AND i.mandate_id = m.mandate_id
				AND i.status IN ('UNSIGNED', 'ACTIVE')), 0),
			COALESCE((SELECT sum(i.amount_minor) FROM steelmatter.issuances i WHERE i.tenant_id = m.tenant_id AND i.mandate_id = m.mandate_id
				AND i.status = 'CONSUMED' AND NOT EXISTS (SELECT 1 FROM steelmatter.dispatches d WHERE d.tenant_id = i.tenant_id AND d.permit_id = i.permit_id)), 0)
			+ COALESCE((SELECT sum(d.amount_minor) FROM steelmatter.dispatches d WHERE d.tenant_id = m.tenant_id AND d.mandate_id = m.mandate_id
				AND d.status = 'COMMITTED'), 0),
			COALESCE((SELECT sum(d.amount_minor) FROM steelmatter.dispatches d WHERE d.tenant_id = m.tenant_id AND d.mandate_id = m.mandate_id
				AND d.status IN ('PENDING', 'UNRESOLVED', 'HELD', 'MISMATCH')), 0)
		FROM steelmatter.mandates m ORDER BY m.tenant_id, m.mandate_id`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tenant, mandate string
		var lim, av, res, com, unr, wantRes, wantCom, wantUnr int64
		if err := rows.Scan(&tenant, &mandate, &lim, &av, &res, &com, &unr, &wantRes, &wantCom, &wantUnr); err != nil {
			return nil, err
		}
		wantAv := lim - wantRes - wantCom - wantUnr
		if wantAv < 0 || av != wantAv || res != wantRes || com != wantCom || unr != wantUnr {
			out = append(out, fmt.Sprintf("%s/%s: stored available %d reserved %d committed %d unresolved %d; rebuilt %d %d %d %d",
				tenant, mandate, av, res, com, unr, wantAv, wantRes, wantCom, wantUnr))
		}
	}
	return out, classify(rows.Err())
}

// problemText is the operator-facing text of a reconciliation problem: the
// fault's own message when there is one (R1-OBS-01), never a bare
// INTERNAL_ERROR for a cause the engine knows.
func problemText(err error) string {
	if f, ok := AsFault(err); ok {
		return f.Message
	}
	if d, ok := AsDependency(err); ok {
		return d.What + " unavailable: " + d.Err.Error()
	}
	return err.Error()
}

// recoveryLockKey serialises reconciliation and the resume gate across
// every engine process sharing the engine database (R1-P4-DEF-01).
const recoveryLockKey int64 = 0x5354_4d52_3152_4543 // "STMR1REC"

// recoveryLock takes the engine-wide recovery lock, waiting for a
// concurrent reconciliation or resume to finish; the second caller then
// sees the first one's results and reports only its own work.
func (e *Engine) recoveryLock(ctx context.Context) (func(), error) {
	conn, err := e.Pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, recoveryLockKey); err != nil {
		conn.Release()
		return nil, unavailable(err)
	}
	return func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, recoveryLockKey)
		conn.Release()
	}, nil
}

// reconcileReserve is the time a reconciliation keeps back before its
// request deadline, so it stops between operations with every completed step
// recorded instead of being cut off inside one (R1-DEF-30).
const reconcileReserve = 10 * time.Second

type budget struct {
	deadline time.Time
	has      bool
}

func newBudget(ctx context.Context) budget {
	d, ok := ctx.Deadline()
	return budget{deadline: d.Add(-reconcileReserve), has: ok}
}

func (b budget) spent() bool { return b.has && time.Now().After(b.deadline) }

// rebuildFromIntent restores the instruction, action revision and issuance
// of an anchored dispatch that an engine restore lost (R1-DEF-05). Nothing
// is taken on trust: the permit's signature is verified against the trust
// store and every field must match the intent; the action canonical must
// reproduce the anchored digest; the mandate must exist (and, at the
// permit's revision, carry the permit's mandate digest). The issuance comes
// back ACTIVE with its amount moved from available to reserved, so the
// normal rebuild then consumes it and holds the amount as unresolved.
// Anything that does not fit is refused with a named cause and nothing is
// written.
func (e *Engine) rebuildFromIntent(ctx context.Context, tx pgx.Tx, ir anchor.IntentRecord, m *mandateRow) error {
	refuse := func(cause, format string, args ...any) error {
		return fault("REVISION_STALE", "REBUILD_REFUSED %s: operation %s: "+format, append([]any{cause, ir.OpRef}, args...)...)
	}
	if ir.PermitJWS == "" {
		return refuse("PERMIT_UNSIGNED", "the anchor's intent carries no signed permit")
	}
	v, verr := contract.Verify(ir.PermitJWS, contract.KindPermit, e.Trust)
	if verr != nil {
		return refuse("PERMIT_INVALID", "the anchored permit does not verify: %s", verr.Detail)
	}
	pm := v.Value.(*contract.Permit)
	switch {
	case v.Digest != ir.PermitDigest:
		return refuse("PERMIT_DIGEST_DIFFERS", "permit digest %s, intent %s", v.Digest, ir.PermitDigest)
	case pm.PermitID != ir.PermitID || pm.OperationID != ir.OpRef || pm.TenantID != ir.Tenant || pm.InstructionID != ir.InstructionID ||
		pm.ActionRevision != ir.ActionRevision || pm.ActionDigest != ir.ActionDigest || pm.MandateID != ir.MandateID ||
		pm.ControlEpoch != ir.ControlEpoch || pm.AmountMinor != ir.AmountMinor || pm.Currency != ir.Currency:
		return refuse("PERMIT_INTENT_DIFFERS", "the signed permit does not match the anchor's intent")
	case m.Revision == pm.MandateRevision && m.Digest != pm.MandateDigest:
		return refuse("MANDATE_DIFFERS", "mandate %s revision %d does not carry the permit's mandate digest", ir.MandateID, m.Revision)
	}
	canonical, digest, cerr := contract.CanonicalDigest(contract.KindAction, []byte(ir.ActionCanonical))
	if cerr != nil || digest != ir.ActionDigest || string(canonical) != ir.ActionCanonical {
		return refuse("ACTION_DIFFERS", "the anchored action does not reproduce digest %s", ir.ActionDigest)
	}
	var act contract.Action
	if err := json.Unmarshal(canonical, &act); err != nil {
		return refuse("ACTION_DIFFERS", "the anchored action does not parse: %v", err)
	}
	if act.TenantID != ir.Tenant || act.InstructionID != ir.InstructionID || act.ActionRevision != ir.ActionRevision ||
		act.AmountMinor != ir.AmountMinor || act.ActorID != pm.ActorID || act.Audience != pm.Audience {
		return refuse("ACTION_DIFFERS", "the anchored action does not match its permit")
	}

	// Instruction. One made after the restore for the same source record
	// (an issuer re-creating the lost instruction) gives the key back to the
	// anchored one, if it never spent (NEW-RT0EE-SEC-01).
	var instMandate, instSuperseded string
	err := tx.QueryRow(ctx, `SELECT mandate_id, COALESCE(superseded_by, '') FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
		ir.Tenant, ir.InstructionID).Scan(&instMandate, &instSuperseded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if ir.SourceSystem == "" || ir.SourceRecordID == "" || ir.IssuerPrincipal == "" || ir.CreationDigest == "" {
			return refuse("INSTRUCTION_MISSING", "instruction %s is not in the engine database and the anchor's intent predates the source fields needed to rebuild it",
				ir.InstructionID)
		}
		var other string
		err := tx.QueryRow(ctx, `SELECT instruction_id FROM steelmatter.instructions WHERE tenant_id = $1 AND source_system = $2 AND source_record_id = $3
			AND superseded_by IS NULL FOR UPDATE`, ir.Tenant, ir.SourceSystem, ir.SourceRecordID).Scan(&other)
		switch {
		case err == nil:
			if err := e.releaseUnspent(ctx, tx, ir, other, 0); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.instructions SET superseded_by = $3, superseded_at = clock_timestamp()
				WHERE tenant_id = $1 AND instruction_id = $2`, ir.Tenant, other, ir.InstructionID); err != nil {
				return err
			}
			if err := recoveryEvent(ctx, tx, ir, "instruction.superseded_by_recovery", map[string]any{
				"instruction_id": other, "superseded_by": ir.InstructionID, "source_system": ir.SourceSystem, "source_record_id": ir.SourceRecordID}); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.instructions (tenant_id, instruction_id, source_system, source_record_id,
			mandate_id, actor_id, issuer_principal, creation_request_digest, current_revision, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp())`,
			ir.Tenant, ir.InstructionID, ir.SourceSystem, ir.SourceRecordID, ir.MandateID, pm.ActorID, ir.IssuerPrincipal, ir.CreationDigest,
			ir.ActionRevision); err != nil {
			return err
		}
	case err != nil:
		return err
	case instMandate != ir.MandateID:
		return refuse("INSTRUCTION_DIFFERS", "instruction %s belongs to mandate %s", ir.InstructionID, instMandate)
	case instSuperseded != "":
		return refuse("INSTRUCTION_DIFFERS", "instruction %s was superseded by %s", ir.InstructionID, instSuperseded)
	}

	// Action revision. The anchored revision becomes the live one. Another
	// revision that is live in the restored database (made before the
	// backup and amended later, NEW-RT0EE-SEC-02, or amended by the issuer
	// after the restore, NEW-RT0EE-SEC-01) is superseded and its open
	// reservation released, if it never spent. Its rows stay as history.
	var revStatus, revDigest string
	err = tx.QueryRow(ctx, `SELECT status, action_digest FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		ir.Tenant, ir.InstructionID, ir.ActionRevision).Scan(&revStatus, &revDigest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		revStatus = ""
	case err != nil:
		return err
	case revDigest != ir.ActionDigest:
		return refuse("REVISION_NUMBER_TAKEN", "action revision %d of instruction %s carries digest %s, not the anchored %s; "+
			"it was amended after the restore to a different action under the same revision number", ir.ActionRevision, ir.InstructionID, revDigest, ir.ActionDigest)
	case revStatus == "COMMITTED":
		return refuse("REVISION_DIFFERS", "action revision %d is COMMITTED without a dispatch", ir.ActionRevision)
	}
	var priorRev int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`,
		ir.Tenant, ir.InstructionID).Scan(&priorRev); err != nil {
		return err
	}
	if err := e.releaseUnspent(ctx, tx, ir, ir.InstructionID, ir.ActionRevision); err != nil {
		return err
	}
	switch revStatus {
	case "":
		if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.action_revisions (tenant_id, instruction_id, action_revision, status, action_canonical,
			action_digest, supplier_id, supplier_revision, delivery_id, audience, amount_minor, created_at)
			VALUES ($1, $2, $3, 'CURRENT', $4, $5, $6, $7, $8, $9, $10, clock_timestamp())`,
			ir.Tenant, ir.InstructionID, ir.ActionRevision, canonical, digest, act.SupplierID, act.SupplierRevision, act.DeliveryID, act.Audience,
			act.AmountMinor); err != nil {
			return err
		}
	case "SUPERSEDED":
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.action_revisions SET status = 'CURRENT' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
			ir.Tenant, ir.InstructionID, ir.ActionRevision); err != nil {
			return err
		}
	}
	if priorRev != ir.ActionRevision {
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.instructions SET current_revision = $3 WHERE tenant_id = $1 AND instruction_id = $2`,
			ir.Tenant, ir.InstructionID, ir.ActionRevision); err != nil {
			return err
		}
	}
	if revStatus != "CURRENT" && revStatus != "" || priorRev != ir.ActionRevision {
		if err := recoveryEvent(ctx, tx, ir, "instruction.revision_restored_by_recovery", map[string]any{
			"instruction_id": ir.InstructionID, "from_revision": priorRev, "to_revision": ir.ActionRevision, "action_digest": ir.ActionDigest}); err != nil {
			return err
		}
	}

	// Issuance: back to ACTIVE with its amount reserved.
	var avail int64
	if err := tx.QueryRow(ctx, `SELECT available_minor FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`, ir.Tenant, ir.MandateID).
		Scan(&avail); err != nil {
		return err
	}
	if avail < ir.AmountMinor {
		return refuse("INSUFFICIENT_BUDGET", "mandate %s has only %d minor units available to hold %d again", ir.MandateID, avail, ir.AmountMinor)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.issuances (tenant_id, permit_id, reservation_id, operation_id, instruction_id,
		action_revision, mandate_id, control_epoch, amount_minor, status, permit_canonical, permit_digest, permit_jws, issued_at, expires_at, signed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'ACTIVE', $10, $11, $12, $13, $14, clock_timestamp())`,
		ir.Tenant, pm.PermitID, pm.ReservationID, pm.OperationID, pm.InstructionID, pm.ActionRevision, pm.MandateID, pm.ControlEpoch,
		pm.AmountMinor, v.Payload, v.Digest, ir.PermitJWS, pm.IssuedAt, pm.ExpiresAt); err != nil {
		if isUnique(err, "issuance_reservation_unique") || isUnique(err, "issuance_operation_unique") {
			return refuse("PERMIT_USED_ELSEWHERE", "the permit's reservation or operation is already used by another issuance")
		}
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE steelmatter.mandates SET available_minor = available_minor - $3, reserved_minor = reserved_minor + $3
		WHERE tenant_id = $1 AND mandate_id = $2`, ir.Tenant, ir.MandateID, ir.AmountMinor)
	return err
}

// instructionSource is the instruction's trusted source key, issuer and
// creation digest, copied into each DISPATCH_INTENT (R1-DEF-05).
type instructionSource struct {
	System, RecordID, Issuer, Digest string
}

func (s instructionSource) apply(ir *anchor.IntentRecord) {
	ir.SourceSystem, ir.SourceRecordID, ir.IssuerPrincipal, ir.CreationDigest = s.System, s.RecordID, s.Issuer, s.Digest
}

// releaseUnspent prepares instruction instID for recovery to replace its
// live state with the anchored action (Aakhil, 30 Oct 2026: automatic only
// when the change is proven never dispatched or executed): every open (UNSIGNED or ACTIVE)
// permit other than the anchored one is released with RECOVERED_UNUSED and
// its amount returned to available, and every CURRENT revision other than
// keepRevision (0: all) is superseded. It refuses, writing nothing, if the
// instruction ever spent or may have spent outside the anchored permit: a
// dispatch (any status, including unresolved or held), a consumed permit, a
// committed revision, a synthetic operation, or another dispatch intent in
// the recovery anchor. A refusal leaves every held or committed amount as it
// is and keeps the resume gate closed for operator review.
func (e *Engine) releaseUnspent(ctx context.Context, tx pgx.Tx, ir anchor.IntentRecord, instID string, keepRevision int64) error {
	var spent string
	err := tx.QueryRow(ctx, `SELECT CASE
		WHEN EXISTS (SELECT 1 FROM steelmatter.dispatches WHERE tenant_id = $1 AND instruction_id = $2) THEN 'a dispatch'
		WHEN EXISTS (SELECT 1 FROM steelmatter.operations WHERE tenant_id = $1 AND instruction_id = $2) THEN 'a synthetic operation'
		WHEN EXISTS (SELECT 1 FROM steelmatter.issuances WHERE tenant_id = $1 AND instruction_id = $2 AND status = 'CONSUMED' AND permit_id <> $3) THEN 'a consumed permit'
		WHEN EXISTS (SELECT 1 FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND status = 'COMMITTED' AND action_revision <> $4) THEN 'a committed revision'
		ELSE '' END`, ir.Tenant, instID, ir.PermitID, keepRevision).Scan(&spent)
	if err != nil {
		return err
	}
	if spent == "" {
		// The anchor is the proof of "never dispatched": any other dispatch
		// intent for this instruction means an order may exist (Aakhil,
		// 30 Oct 2026: dispatched, spent or uncertain stops for review).
		ops, aerr := e.Anchor.IntentOpsForInstruction(ctx, ir.Tenant, instID)
		if aerr != nil {
			return dependency("recovery anchor", aerr)
		}
		for _, op := range ops {
			if op != ir.OpRef {
				spent = "a dispatch intent in the recovery anchor (" + op + ")"
				break
			}
		}
	}
	if spent != "" {
		return fault("REVISION_STALE", "REBUILD_REFUSED CONFLICT_SPENT: operation %s: instruction %s, made or changed after the restore, already has %s; "+
			"nothing was superseded, released or sent; an operator must review it",
			ir.OpRef, instID, spent)
	}
	rows, err := tx.Query(ctx, `SELECT permit_id, mandate_id, amount_minor FROM steelmatter.issuances
		WHERE tenant_id = $1 AND instruction_id = $2 AND status IN ('UNSIGNED', 'ACTIVE') AND permit_id <> $3 ORDER BY permit_id FOR UPDATE`,
		ir.Tenant, instID, ir.PermitID)
	if err != nil {
		return err
	}
	type hold struct {
		permit, mandate string
		amount          int64
	}
	var holds []hold
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.permit, &h.mandate, &h.amount); err != nil {
			rows.Close()
			return err
		}
		holds = append(holds, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range holds {
		if h.mandate != ir.MandateID {
			if _, err := lockMandate(ctx, tx, ir.Tenant, h.mandate, "FOR UPDATE"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'RECOVERED_UNUSED', released_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2`, ir.Tenant, h.permit); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
			WHERE tenant_id = $1 AND mandate_id = $2`, ir.Tenant, h.mandate, h.amount); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE steelmatter.action_revisions SET status = 'SUPERSEDED'
		WHERE tenant_id = $1 AND instruction_id = $2 AND status = 'CURRENT' AND action_revision <> $3`, ir.Tenant, instID, keepRevision)
	return err
}

// recoveryEvent records what reconciliation changed beyond the rebuilt
// dispatch, for the audit trail.
func recoveryEvent(ctx context.Context, tx pgx.Tx, ir anchor.IntentRecord, eventType string, detail map[string]any) error {
	detail["op_ref"] = ir.OpRef
	payload, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp())`, newID("evt"), ir.Tenant, eventType, ir.OpRef, payload)
	return err
}
