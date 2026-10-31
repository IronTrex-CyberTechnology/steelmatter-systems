//go:build integration && failpoints

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/dbtest"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// D5 v3 with the real PostgreSQL recovery anchor (decided 28 Oct 2026:
// option 1, a separate PostgreSQL tested locally). The engine database is
// restored at database level (a point-in-time copy of the whole database,
// dropped and recreated), the destination through the stub's backup, whose
// restore-proof records live in the real anchor's destination stream. The
// anchor is never restored.

var (
	uday   = auth.Principal{ID: "uday", Tenant: tenant, Kind: auth.Operator}
	kaif   = auth.Principal{ID: "kaif", Tenant: tenant, Kind: auth.Operator}
	aakhil = auth.Principal{ID: "aakhil", Tenant: tenant, Kind: auth.Operator}
)

type anchored struct {
	e  *env
	ah *dbtest.AnchorHarness
	c  *anchor.Client
	s  *stub.Stub
}

func withAnchor(t *testing.T, e *env) *anchored {
	t.Helper()
	ah := dbtest.NewAnchor(t)
	a := &anchored{e: e, ah: ah, c: &anchor.Client{Pool: ah.Engine}}
	a.s = stub.NewWithProof(&anchor.DestStream{Pool: ah.Dest, Destination: e.eng.Cfg.CompanyRef})
	e.eng.Cfg.RestoreGrace = 0
	if _, err := e.eng.UseAnchor(context.Background(), a.c); err != nil {
		t.Fatal(err)
	}
	e.eng.Connector = a.s
	e.eng.RepairReviewers, e.eng.RepairConfirmers = []string{"kaif"}, []string{"aakhil"}
	return a
}

func (a *anchored) count(typ string) int {
	a.e.t.Helper()
	var n int
	if err := a.ah.Admin.QueryRow(context.Background(), `SELECT count(*) FROM anchor.engine_records WHERE record_type = $1`, typ).Scan(&n); err != nil {
		a.e.t.Fatal(err)
	}
	return n
}

func (a *anchored) total() int {
	a.e.t.Helper()
	var n int
	_ = a.ah.Admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM anchor.engine_records) + (SELECT count(*) FROM anchor.destination_records)`).Scan(&n)
	return n
}

func (a *anchored) verify() {
	a.e.t.Helper()
	if err := anchor.Verify(context.Background(), a.ah.Engine); err != nil {
		a.e.t.Fatal(err)
	}
}

func (a *anchored) writes() anchor.WriteState {
	a.e.t.Helper()
	ws, err := a.c.WriteState(context.Background())
	if err != nil {
		a.e.t.Fatal(err)
	}
	return ws
}

func (a *anchored) check() *engine.RestoreReport {
	a.e.t.Helper()
	r, err := a.e.eng.CheckRestore(context.Background(), a.s)
	if err != nil {
		a.e.t.Fatal(err)
	}
	return r
}

func (a *anchored) reconcile() *engine.ReconcileReport {
	a.e.t.Helper()
	r, err := a.e.eng.ReconcileRestore(context.Background(), a.s)
	if err != nil {
		a.e.t.Fatal(err)
	}
	return r
}

func (a *anchored) gate() *engine.GateReport {
	a.e.t.Helper()
	r, err := a.e.eng.TryResume(context.Background(), a.s)
	if err != nil {
		a.e.t.Fatal(err)
	}
	return r
}

func hasSignal(r *engine.RestoreReport, code string) bool {
	for _, s := range r.Signals {
		if s.Code == code {
			return true
		}
	}
	return false
}

func gateCheck(r *engine.GateReport, name string) engine.GateCheck {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return engine.GateCheck{Name: name}
}

// ---- database-level restore of the engine database ----

// snapshotEngine takes a point-in-time copy of the whole engine database.
func (e *env) snapshotEngine() string {
	e.t.Helper()
	ctx := context.Background()
	snap := e.h.DB.Name + "_snap" + fmt.Sprint(time.Now().UnixNano()%100000)
	admin := e.clusterAdmin()
	defer admin.Close(ctx)
	e.dropEngineConnections(admin)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{snap}.Sanitize()+" TEMPLATE "+pgx.Identifier{e.h.DB.Name}.Sanitize()); err != nil {
		e.t.Fatalf("snapshot: %v", err)
	}
	e.t.Cleanup(func() {
		c := e.clusterAdmin()
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{snap}.Sanitize()+" WITH (FORCE)")
	})
	return snap
}

// restoreEngine replaces the engine database with the snapshot, as restoring
// a backup would. The anchor database is not touched.
func (e *env) restoreEngine(snap string) {
	e.t.Helper()
	ctx := context.Background()
	admin := e.clusterAdmin()
	defer admin.Close(ctx)
	e.dropEngineConnections(admin)
	name := pgx.Identifier{e.h.DB.Name}.Sanitize()
	for _, sql := range []string{
		"DROP DATABASE " + name + " WITH (FORCE)",
		"CREATE DATABASE " + name + " TEMPLATE " + pgx.Identifier{snap}.Sanitize(),
		"REVOKE CONNECT, TEMPORARY ON DATABASE " + name + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + name + " TO " + pgx.Identifier{e.h.DB.ExecutorRole}.Sanitize() + ", " + pgx.Identifier{e.h.DB.ObserverRole}.Sanitize(),
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			e.t.Fatalf("restore: %v\n%s", err, sql)
		}
	}
}

func (e *env) clusterAdmin() *pgx.Conn {
	e.t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv(dbtest.EnvAdminURL))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) dropEngineConnections(admin *pgx.Conn) {
	e.t.Helper()
	for _, p := range []interface{ Reset() }{e.h.Admin, e.h.Executor, e.h.Observer} {
		p.Reset()
	}
	if _, err := admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		e.h.DB.Name); err != nil {
		e.t.Fatal(err)
	}
}

// ---- anchor records ----

// Dispatch records the full intent in the anchor before committing, every
// destination result is recorded, and the chain verifies.
func TestD5DispatchAndOutcomesAreRecordedInTheAnchor(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	intents, err := a.c.Intents(context.Background())
	if err != nil || len(intents) != 1 {
		t.Fatalf("intents %v (%v)", intents, err)
	}
	ir := intents[0]
	if ir.OpRef != p.operation || ir.MandateID != p.mandate || ir.PermitJWS != p.jws || ir.AmountMinor != sixLakh || ir.ActionCanonical == "" ||
		ir.PermitDigest == "" || ir.CompanyRef == "" || ir.SupplierRef != "supplier-a" || ir.DeliveryRef != "warehouse-a" {
		t.Fatalf("intent %+v", ir)
	}
	a.s.DropNextReplies(p.operation, 1)
	e.settle(p.operation, a.s)
	e.settle(p.operation, a.s)
	outs, _ := a.c.LatestOutcomes(context.Background())
	if o := outs[[2]string{tenant, p.operation}]; o.Status != engine.DispatchCommitted || o.Move != "unresolved->committed" || o.DestinationOrder == "" {
		t.Fatalf("outcome %+v", o)
	}
	// The DISPATCHED marker after the dispatch commit (R1-DEF-06), then the
	// lost-reply UNKNOWN and the COMMITTED outcome.
	if a.count(anchor.Outcome) != 3 {
		t.Fatalf("outcomes %d", a.count(anchor.Outcome))
	}
	var marker string
	if err := a.ah.Admin.QueryRow(context.Background(), `SELECT payload->>'outcome' FROM anchor.engine_records WHERE record_type = 'OUTCOME' ORDER BY seq LIMIT 1`).Scan(&marker); err != nil || marker != "DISPATCHED" {
		t.Fatalf("first outcome %q (%v)", marker, err)
	}
	if n, _ := a.c.ExpectedMinSeq(context.Background(), e.eng.Cfg.CompanyRef); n != a.s.Head() {
		t.Fatalf("expected min seq %d, destination head %d", n, a.s.Head())
	}
	a.verify()
	if r := a.check(); len(r.Signals) != 0 || !a.writes().Enabled {
		t.Fatalf("a healthy system reported %v", r.Signals)
	}
}

// No anchor record, no dispatch: when the anchor cannot take the intent the
// dispatch rolls back with nothing consumed or moved.
func TestD5AnchorUnavailableRefusesDispatch(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	if _, err := a.ah.Admin.Exec(ctx, `REVOKE EXECUTE ON FUNCTION anchor.append_engine(text, text, text, jsonb, bigint) FROM anchor_engine_writer`); err != nil {
		t.Fatal(err)
	}
	_, err := e.dispatch("agent-a", p)
	if f, ok := engine.AsFault(err); !ok || f.Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("dispatch with the anchor unavailable: %v", err)
	}
	if st, _ := e.issuanceStatus(p.permitID); st != "ACTIVE" || e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 {
		t.Fatalf("permit %s, dispatches %d", st, e.count(`SELECT count(*) FROM steelmatter.dispatches`))
	}
	requireBuckets(t, "nothing moved", e.buckets(p.mandate), r1Reserved)
	if _, err := a.ah.Admin.Exec(ctx, `GRANT EXECUTE ON FUNCTION anchor.append_engine(text, text, text, jsonb, bigint) TO anchor_engine_writer`); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, e, p)
	// Settling also needs the anchor: an outcome the anchor cannot record is
	// not recorded in the engine either, and the amount stays held.
	if _, err := a.ah.Admin.Exec(ctx, `REVOKE EXECUTE ON FUNCTION anchor.append_engine(text, text, text, jsonb, bigint) FROM anchor_engine_writer`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.Settle(ctx, tenant, p.operation, a.s); err == nil {
		t.Fatal("settle recorded an outcome the anchor could not record")
	}
	if e.dispatchStatus(p.operation) != engine.DispatchPending {
		t.Fatalf("status %s", e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
}

// While writes are off nothing new is created: dispatch is refused, and a
// PENDING operation goes to lookup and fence instead of execute.
func TestD5WritesOffStopsCreatesButNotFences(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	q := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	mustDispatch(t, e, p)
	if _, err := a.c.DisableWrites(context.Background(), "TEST"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dispatch("agent-a", q); err == nil {
		t.Fatal("dispatch while writes are off")
	}
	requireBuckets(t, "q untouched", e.buckets(q.mandate), r1Reserved)
	if d, err := e.dispatch("agent-a", p); err != nil || d.OpRef != p.operation {
		t.Fatalf("an identical repeat still returns the stored dispatch: %+v %v", d, err)
	}
	requireExecution(t, "settle while off", e.settle(p.operation, a.s), engine.DispatchFenced)
	if a.s.Creates() != 0 {
		t.Fatal("an order was created while writes were off")
	}
	requireBuckets(t, "released by the fence", e.buckets(p.mandate), r1Released)
}

// A process from before a restore keeps its old generation and is refused;
// the process that ran the resume gate dispatches.
func TestD5StaleGenerationIsRefused(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	trust, signers, err := contract.LoadKeyring(e.keysDir)
	if err != nil {
		t.Fatal(err)
	}
	es, _, _ := contract.SplitObserver(signers)
	stale, err := engine.New(e.h.Executor, trust, es, e.eng.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.UseAnchor(context.Background(), a.c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.c.DisableWrites(context.Background(), "TEST"); err != nil {
		t.Fatal(err)
	}
	if g := a.gate(); !g.Resumed || g.WriteState.Generation != 2 {
		t.Fatalf("gate %+v", g)
	}
	p := e.permitted()
	action, _ := json.Marshal(p.action)
	_, err = stale.Dispatch(context.Background(), auth.Principal{ID: "agent-a", Tenant: tenant, Kind: auth.Actor}, action, p.jws)
	if f, ok := engine.AsFault(err); !ok || f.Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("stale process dispatched: %v", err)
	}
	mustDispatch(t, e, p)
	if _, err := stale.AdoptWriteState(context.Background()); err != nil || stale.WriteGeneration() != 2 {
		t.Fatal("adopting the generation")
	}
}

// ---- restores ----

// Case A (T35): the engine database is restored to before a confirmed
// purchase. Writes stop; reconciliation rebuilds it from the anchor and the
// destination confirms it; the gate passes; writes resume. The anchor's
// history is untouched by the restore.
func TestD5EngineRestoredBeforeConfirmedPurchase(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	q := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	snap := e.snapshotEngine()
	mustDispatch(t, e, p)
	requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
	before := a.total()

	e.restoreEngine(snap)
	requireBuckets(t, "restored engine forgot the purchase", e.buckets(p.mandate), r1Reserved)
	if a.total() != before {
		t.Fatal("restoring the engine changed the anchor")
	}
	a.verify()
	r := a.check()
	if !r.WritesDisabled || !hasSignal(r, engine.SignalEngineMissingOperation) || a.writes().Enabled {
		t.Fatalf("restore not detected: %+v", r)
	}
	st, body := e.evaluate("agent-a", q.instruction, 1)
	requireStatus(t, "evaluate while writes are off", st, body, 503, "SERVICE_UNAVAILABLE")
	if _, err := e.dispatch("agent-a", q); err == nil {
		t.Fatal("dispatch while writes are off")
	}
	if g := a.gate(); g.Passed {
		t.Fatal("the gate passed before reconciliation")
	}
	rec := a.reconcile()
	if len(rec.Recreated) != 1 || len(rec.Problems) != 0 || e.dispatchStatus(p.operation) != engine.DispatchCommitted {
		t.Fatalf("reconcile %+v, status %s", rec, e.dispatchStatus(p.operation))
	}
	if a.s.Creates() != 1 {
		t.Fatalf("creates %d", a.s.Creates())
	}
	requireBuckets(t, "committed again", e.buckets(p.mandate), r1Committed)
	if g := a.gate(); !g.Resumed || !a.writes().Enabled {
		t.Fatalf("gate %+v", g)
	}
	mustDispatch(t, e, q)
	a.verify()
}

// Case B: restored to before a dispatch that never reached the destination.
// It is rebuilt as UNRESOLVED and released only after the fence proves no
// order; a late create is then refused by the anchor's BLOCK.
func TestD5EngineRestoredBeforeUnsentDispatch(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	snap := e.snapshotEngine()
	mustDispatch(t, e, p) // recorded, never sent
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Recreated) != 1 || e.dispatchStatus(p.operation) != engine.DispatchFenced {
		t.Fatalf("reconcile %+v, status %s", rec, e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "released after the fence", e.buckets(p.mandate), r1Released)
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	if r := a.s.Deliver(stubActionFor(t, e, p)); r.Outcome != connector.RefusedFenced || a.s.Creates() != 0 {
		t.Fatalf("late create: %s", r.Outcome)
	}
}

// Case G with the HELD rule: the engine and Odoo are both restored after a
// lost-reply create whose CREATE_DONE was lost. The operation is rebuilt and
// ends HELD; writes stay off until Uday records the finding, then resume
// while it stays held. Only Kaif's review plus Aakhil's confirmation, after
// the recovery checks pass, moves the amount.
func TestD5BothRestoredHeldRuleAndRepair(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	snap := e.snapshotEngine()
	destBefore := a.s.Backup()
	mustDispatch(t, e, p)
	a.s.DropNextReplies(p.operation, 1)
	a.s.LoseNextCreateDone(p.operation, 1)
	requireExecution(t, "lost reply", e.settle(p.operation, a.s), engine.DispatchUnresolved)
	e.restoreEngine(snap)
	a.s.Restore(destBefore)

	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Recreated) != 1 || e.dispatchStatus(p.operation) != engine.DispatchHeld {
		t.Fatalf("reconcile %+v, status %s", rec, e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)

	// Repairs are refused before the recovery checks pass.
	if _, err := e.eng.RecordHeldFinding(ctx, auth.Principal{ID: "agent-a", Tenant: tenant, Kind: auth.Actor}, p.operation, "x", "y"); err == nil {
		t.Fatal("a non-operator recorded a finding")
	}
	g := a.gate()
	if g.Passed || gateCheck(g, "HELD_RULE").Passed || a.writes().Enabled {
		t.Fatalf("gate passed without a HELD finding: %+v", g)
	}
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "Odoo restore erased an order created after a lost reply; outcome unknown",
		"incident-2026-10-28/odoo-backup-and-supplier-email"); err != nil {
		t.Fatal(err)
	}
	requireBuckets(t, "a finding alone moves nothing", e.buckets(p.mandate), r1Unresolved)
	repairID, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairCommit, "supplier-confirmation-PO-erased.pdf", "supplier confirmed delivery")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, repairID, true, "evidence checked against Odoo logs"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, repairID, a.s); err == nil {
		t.Fatal("a repair was confirmed while writes were off")
	}

	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate with the HELD rule met: %+v", g)
	}
	if e.dispatchStatus(p.operation) != engine.DispatchHeld {
		t.Fatal("resuming writes settled a HELD operation")
	}
	requireBuckets(t, "still held after resume", e.buckets(p.mandate), r1Unresolved)

	// Separation of duties.
	if err := e.eng.ReviewRepair(ctx, uday, p.operation, repairID, true, "self"); err == nil {
		t.Fatal("the preparer reviewed")
	}
	if _, err := e.eng.ConfirmRepair(ctx, kaif, p.operation, repairID, a.s); err == nil {
		t.Fatal("the reviewer confirmed")
	}
	e.eng.RepairConfirmers = []string{"aakhil", "kaif"}
	if _, err := e.eng.ConfirmRepair(ctx, kaif, p.operation, repairID, a.s); err == nil {
		t.Fatal("a confirmer who also reviewed confirmed")
	}
	e.eng.RepairConfirmers = []string{"aakhil"}

	d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, repairID, a.s)
	if err != nil || d.Execution != engine.DispatchCommitted {
		t.Fatalf("confirm: %+v %v", d, err)
	}
	requireBuckets(t, "committed by the confirmed repair", e.buckets(p.mandate), r1Committed)
	if d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, repairID, a.s); err != nil || d.Execution != engine.DispatchCommitted {
		t.Fatalf("retry must return the same confirmed result: %+v %v", d, err)
	}
	if a.count(anchor.RepairConfirmed) != 1 || a.count(anchor.OperatorFinding) != 1 {
		t.Fatal("anchor repair records")
	}
	a.verify()
}

// A repair that releases: an unreviewed or rejected repair never moves
// budget; an approved one confirmed by Aakhil releases the held amount, and
// only because the anchor proves no create was attempted (decision 3).
func TestD5RepairReleaseNeedsApprovedReview(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	mustDispatch(t, e, p)
	e.heldNeverSent(p.operation, a.s)
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "held after restore", "ticket-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.PrepareRepair(ctx, uday, p.operation, "REFUND", "e", ""); err == nil {
		t.Fatal("unknown repair outcome")
	}
	id, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairRelease, "supplier-says-no-order.pdf", "no order received")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err == nil {
		t.Fatal("confirmed without a review")
	}
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, false, "evidence incomplete"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err == nil {
		t.Fatal("confirmed a rejected repair")
	}
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "now complete"); err != nil {
		t.Fatal(err)
	}
	if d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err != nil || d.Execution != engine.DispatchFenced {
		t.Fatalf("confirm: %+v %v", d, err)
	}
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
}

// Case E: Odoo alone is restored and a committed order disappears. Writes
// stop; the operation gets a BLOCK and a flag but stays committed (no
// refund); writes resume once Uday records the finding; a retry can never
// create a second order.
func TestD5OdooRestoredCommittedOrderMissing(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	destBefore := a.s.Backup()
	mustDispatch(t, e, p)
	requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
	a.s.Restore(destBefore)
	r := a.check()
	if !r.WritesDisabled || !hasSignal(r, engine.SignalCommittedOrderMissing) || !hasSignal(r, engine.SignalDestinationBehind) {
		t.Fatalf("signals %+v", r.Signals)
	}
	rec := a.reconcile()
	if len(rec.Flagged) != 1 || len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if e.dispatchStatus(p.operation) != engine.DispatchCommitted ||
		e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = $1 AND flag = 'ORDER_MISSING_AFTER_RESTORE'`, p.operation) != 1 {
		t.Fatal("expected a flagged committed operation")
	}
	requireBuckets(t, "no refund", e.buckets(p.mandate), r1Committed)
	if g := a.gate(); g.Passed {
		t.Fatal("gate passed without a finding for the missing order")
	}
	if _, err := e.eng.RecordHeldFinding(context.Background(), uday, p.operation, "order erased by an Odoo restore", "odoo-restore-log"); err != nil {
		t.Fatal(err)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	if r := a.s.Deliver(stubActionFor(t, e, p)); !r.Outcome.Tombstones() || a.s.Creates() != 1 {
		t.Fatalf("retry after the restore: %s, creates %d", r.Outcome, a.s.Creates())
	}
	if r := a.check(); len(r.Signals) != 0 {
		t.Fatalf("signals after resume %+v", r.Signals)
	}
}

// Case 2: a destination restore removes a committed block; the delayed
// request is refused by the anchor's BLOCK; the restore is detected and the
// fence re-applied; writes resume with a new destination baseline.
func TestD5OdooRestoredAfterFenceDelayedRequestRefused(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	a.s.FailNextCalls(p.operation, 1)
	e.settle(p.operation, a.s)
	destBefore := a.s.Backup()
	requireExecution(t, "fence", e.settle(p.operation, a.s), engine.DispatchFenced)
	a.s.Restore(destBefore)
	if r := a.s.Deliver(stubActionFor(t, e, p)); r.Outcome != connector.RefusedFenced || a.s.Creates() != 0 {
		t.Fatalf("delayed request: %s", r.Outcome)
	}
	// The refusal re-wrote the tombstone; restore once more to test detection.
	a.s.Restore(destBefore)
	if r := a.check(); !r.WritesDisabled || !hasSignal(r, engine.SignalDestinationBehind) {
		t.Fatalf("signals %+v", r.Signals)
	}
	rec := a.reconcile()
	if len(rec.Refenced) != 1 || len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if g := a.gate(); !g.Resumed || g.WriteState.BaselineSeq != a.s.Head() {
		t.Fatalf("gate %+v (head %d)", g, a.s.Head())
	}
	requireBuckets(t, "still released", e.buckets(p.mandate), r1Released)
}

// The anchor behind the engine database: writes stop, and the gate fails
// until Aakhil confirms the anchor repair.
func TestD5AnchorBehindNeedsConfirmedRepair(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p) // before the anchor was attached
	a := withAnchor(t, e)
	r := a.check()
	if !r.WritesDisabled || !hasSignal(r, engine.SignalAnchorMissingOperation) {
		t.Fatalf("signals %+v", r.Signals)
	}
	a.reconcile()
	if g := a.gate(); g.Passed || gateCheck(g, "ANCHOR_NOT_BEHIND").Passed {
		t.Fatalf("gate %+v", g)
	}
	// Decision 4: Uday prepares, Kaif reviews, Aakhil confirms.
	ctx := context.Background()
	id, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairAnchor, "engine-db-export-2026-10-28", "anchor behind")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.eng.ConfirmAnchorRepair(ctx, aakhil, p.operation, id); err == nil {
		t.Fatal("anchor repaired without a review")
	}
	if err := e.eng.ReviewRepair(ctx, uday, p.operation, id, true, "self"); err == nil {
		t.Fatal("the preparer reviewed")
	}
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "export matches"); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.ConfirmAnchorRepair(ctx, kaif, p.operation, id); err == nil {
		t.Fatal("a non-confirmer repaired the anchor")
	}
	e.eng.RepairConfirmers = []string{"aakhil", "kaif"}
	if err := e.eng.ConfirmAnchorRepair(ctx, kaif, p.operation, id); err == nil {
		t.Fatal("the reviewer confirmed")
	}
	e.eng.RepairConfirmers = []string{"aakhil"}
	if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err == nil {
		t.Fatal("an anchor repair was confirmed as a budget repair")
	}
	if err := e.eng.ConfirmAnchorRepair(ctx, aakhil, p.operation, id); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.ConfirmAnchorRepair(ctx, aakhil, p.operation, id); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if _, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairAnchor, "again", ""); err == nil {
		t.Fatal("prepared an anchor repair for an operation the anchor already has")
	}
	// The repaired intent carries the same action facts Dispatch records.
	var sup, cur, del, rev string
	if err := a.ah.Admin.QueryRow(context.Background(), `SELECT payload->>'supplier_ref', payload->>'currency', payload->>'delivery_ref',
		payload->>'supplier_revision' FROM anchor.engine_records WHERE record_type = 'DISPATCH_INTENT' AND op_ref = $1`, p.operation).Scan(&sup, &cur, &del, &rev); err != nil {
		t.Fatal(err)
	}
	if sup != "supplier-a" || cur != "INR" || del != "warehouse-a" || rev != "1" {
		t.Fatalf("repaired intent supplier %q currency %q delivery %q revision %q", sup, cur, del, rev)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	a.verify()
}

// The gate refuses when the rebuilt buckets do not match the stored ones.
func TestD5GateRefusesMismatchedBuckets(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	e.settle(p.operation, a.s)
	if _, err := a.c.DisableWrites(context.Background(), "TEST"); err != nil {
		t.Fatal(err)
	}
	e.adminBypass(`UPDATE steelmatter.mandates SET committed_minor = committed_minor - 1, available_minor = available_minor + 1 WHERE mandate_id = $1`, p.mandate)
	if g := a.gate(); g.Passed || gateCheck(g, "BUCKETS_RECONCILE").Passed {
		t.Fatalf("gate %+v", g)
	}
}

// ---- concurrency ----

// Many dispatches and settles at once on one mandate: every engine operation
// has exactly one anchor intent, the chain verifies, and the buckets match.
func TestD5ConcurrentDispatchAndSettleWithAnchor(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	var ops []permitted
	for i := 0; i < 16; i++ {
		inst := e.instruction(fmt.Sprintf("po-conc-%02d", i), m, line("item-a", 1, 1_000_000))
		id := inst["instruction_id"].(string)
		st, body := e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
		permit := body["permit"].(map[string]any)
		ops = append(ops, permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
			permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)})
	}
	var wg sync.WaitGroup
	for i, p := range ops {
		wg.Add(1)
		go func(i int, p permitted) {
			defer wg.Done()
			action, _ := json.Marshal(p.action)
			if _, err := e.eng.Dispatch(context.Background(), auth.Principal{ID: "agent-a", Tenant: tenant, Kind: auth.Actor}, action, p.jws); err != nil {
				t.Errorf("dispatch %d: %v", i, err)
				return
			}
			if i%3 == 0 {
				a.s.DropNextReplies(p.operation, 1)
			}
			for j := 0; j < 3; j++ {
				_, _ = e.eng.Settle(context.Background(), tenant, p.operation, a.s)
			}
		}(i, p)
	}
	// Two settle workers alongside.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.eng.SettleDue(context.Background()) }()
	}
	wg.Wait()
	if a.count(anchor.DispatchIntent) != len(ops) || e.count(`SELECT count(*) FROM steelmatter.dispatches`) != len(ops) {
		t.Fatalf("intents %d, dispatches %d", a.count(anchor.DispatchIntent), e.count(`SELECT count(*) FROM steelmatter.dispatches`))
	}
	e.requireBudgetMatchesRecords("after the burst", m)
	a.verify()
	if r := a.check(); len(r.Signals) != 0 {
		t.Fatalf("signals %+v", r.Signals)
	}
}

// Turning writes off while dispatches are running: every recorded dispatch
// has its anchor intent, nothing is dispatched after the switch, and no
// intent lands after it.
func TestD5DisableWritesRacingDispatches(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	var ops []permitted
	for i := 0; i < 20; i++ {
		inst := e.instruction(fmt.Sprintf("po-race-%02d", i), m, line("item-a", 1, 1_000_000))
		id := inst["instruction_id"].(string)
		st, body := e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
		permit := body["permit"].(map[string]any)
		ops = append(ops, permitted{action: body["action"].(map[string]any), jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string),
			operation: permit["operation_id"].(string)})
	}
	var wg sync.WaitGroup
	var refused int
	var mu sync.Mutex
	for i, p := range ops {
		wg.Add(1)
		go func(i int, p permitted) {
			defer wg.Done()
			if i == 10 {
				_, _ = a.c.DisableWrites(context.Background(), "TEST")
			}
			action, _ := json.Marshal(p.action)
			if _, err := e.eng.Dispatch(context.Background(), auth.Principal{ID: "agent-a", Tenant: tenant, Kind: auth.Actor}, action, p.jws); err != nil {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		}(i, p)
	}
	wg.Wait()
	intents, dispatches := a.count(anchor.DispatchIntent), e.count(`SELECT count(*) FROM steelmatter.dispatches`)
	if intents != dispatches || dispatches+refused != len(ops) || refused == 0 {
		t.Fatalf("intents %d, dispatches %d, refused %d", intents, dispatches, refused)
	}
	var late int
	_ = a.ah.Admin.QueryRow(context.Background(), `SELECT count(*) FROM anchor.engine_records WHERE record_type = 'DISPATCH_INTENT'
		AND seq > (SELECT seq FROM anchor.engine_records WHERE record_type = 'WRITE_STATE')`).Scan(&late)
	if late != 0 {
		t.Fatalf("%d intents after writes went off", late)
	}
	e.requireBudgetMatchesRecords("after the race", m)
}

// With the normal grace period, a restore check running during a burst of
// dispatches never mistakes an in-flight dispatch for a restore.
func TestD5RestoreCheckDuringDispatchBurstHasNoFalseAlarm(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	e.eng.Cfg.RestoreGrace = time.Minute
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	var ops []permitted
	for i := 0; i < 12; i++ {
		inst := e.instruction(fmt.Sprintf("po-burst-%02d", i), m, line("item-a", 1, 1_000_000))
		id := inst["instruction_id"].(string)
		st, body := e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
		ops = append(ops, permitted{action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
			operation: body["permit"].(map[string]any)["operation_id"].(string)})
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var checkErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if r, err := e.eng.CheckRestore(context.Background(), a.s); err != nil || len(r.Signals) != 0 {
				checkErr = fmt.Errorf("check during burst: %v %+v", err, r)
				return
			}
		}
	}()
	var dwg sync.WaitGroup
	for _, p := range ops {
		dwg.Add(1)
		go func(p permitted) {
			defer dwg.Done()
			action, _ := json.Marshal(p.action)
			if _, err := e.eng.Dispatch(context.Background(), auth.Principal{ID: "agent-a", Tenant: tenant, Kind: auth.Actor}, action, p.jws); err == nil {
				_, _ = e.eng.Settle(context.Background(), tenant, p.operation, a.s)
			}
		}(p)
	}
	dwg.Wait()
	close(stop)
	wg.Wait()
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	if !a.writes().Enabled {
		t.Fatal("writes were turned off by a false alarm")
	}
}

// Two confirmations of one repair at once: the amount moves once.
func TestD5ConcurrentRepairConfirmationsMoveOnce(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	mustDispatch(t, e, p)
	e.heldNeverSent(p.operation, a.s)
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "f", "ev"); err != nil {
		t.Fatal(err)
	}
	id, _ := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairRelease, "ev", "s")
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "ok"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); errs <- err }()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if f, isFault := engine.AsFault(err); !isFault || f.Code != "ALREADY_COMMITTED" {
			if !errors.Is(err, context.Canceled) {
				t.Logf("refused: %v", err)
			}
		}
	}
	if ok != 4 {
		t.Fatalf("%d idempotent confirmation requests succeeded", ok)
	}
	requireBuckets(t, "released once", e.buckets(p.mandate), r1Released)
	if a.count(anchor.RepairConfirmed) != 1 {
		t.Fatalf("repair records %d", a.count(anchor.RepairConfirmed))
	}
}

// destActionForPermit builds the canonical action Odoo would hold for p's
// permit, straight from the engine's issuance and action revision.
func (e *env) destActionForPermit(p permitted) connector.Action {
	e.t.Helper()
	var digest string
	var canonical []byte
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT i.permit_digest, a.action_canonical FROM steelmatter.issuances i
		JOIN steelmatter.action_revisions a ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.action_revision
		WHERE i.operation_id = $1`, p.operation).Scan(&digest, &canonical); err != nil {
		e.t.Fatal(err)
	}
	var act contract.Action
	if err := json.Unmarshal(canonical, &act); err != nil {
		e.t.Fatal(err)
	}
	a := connector.Action{OpRef: p.operation, ActionRevision: act.ActionRevision, CompanyRef: e.eng.Cfg.CompanyRef, DeliveryRef: act.DeliveryID,
		SupplierRef: act.SupplierID, SupplierRevision: act.SupplierRevision, Currency: act.Currency, AmountMinor: act.AmountMinor, PermitDigest: digest,
		AmountUntaxedMinor: act.AmountUntaxedMinor, TaxMinor: act.TaxMinor, DeliveryRevision: act.DeliveryRevision}
	for _, l := range act.Lines {
		a.Lines = append(a.Lines, connector.Line{ItemRef: l.ItemID, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor, UnitRef: l.UnitID, TaxRefs: l.TaxIDs})
	}
	return a
}

// Case C: Odoo holds an order that neither the anchor nor the engine knows
// (both lost it). It is rebuilt from the engine's own permit for that
// op_ref, verified against the order and committed; writes stay off until
// Aakhil confirms the anchor repair, then resume.
func TestD5CaseCOrderOnlyOdooKnowsIsRebuilt(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	if r := a.s.Deliver(e.destActionForPermit(p)); r.Outcome != connector.Created {
		t.Fatalf("order at the destination: %s", r.Outcome)
	}
	r := a.check()
	if !r.WritesDisabled || !hasSignal(r, engine.SignalUnknownDestinationOp) {
		t.Fatalf("signals %+v", r.Signals)
	}
	rec := a.reconcile()
	if len(rec.FromOdoo) != 1 || rec.FromOdoo[0] != p.operation || len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if e.dispatchStatus(p.operation) != engine.DispatchCommitted || a.s.Creates() != 1 {
		t.Fatalf("status %s, creates %d", e.dispatchStatus(p.operation), a.s.Creates())
	}
	requireBuckets(t, "committed from the permit's reservation", e.buckets(p.mandate), r1Committed)
	if g := a.gate(); g.Passed || gateCheck(g, "ANCHOR_NOT_BEHIND").Passed || !gateCheck(g, "ALL_OPERATIONS_PRESENT").Passed {
		t.Fatalf("gate before the anchor repair %+v", g)
	}
	if err := e.anchorRepair(p.operation, "odoo-order-and-engine-permit-match"); err != nil {
		t.Fatal(err)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	a.verify()
}

// Case C refused: an order whose permit digest does not match the engine's
// permit, or whose op_ref has no permit at all, is never attributed; the
// gate stays closed for an operator.
func TestD5CaseCUnattributableOrdersKeepTheGateClosed(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	forged := e.destActionForPermit(p)
	forged.PermitDigest = strings.Repeat("ab", 32)
	a.s.Deliver(forged)
	stranger := e.destActionForPermit(p)
	stranger.OpRef = "op-not-issued-by-this-engine"
	a.s.Deliver(stranger)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("unknown orders not detected")
	}
	rec := a.reconcile()
	if len(rec.FromOdoo) != 0 || len(rec.Problems) != 2 {
		t.Fatalf("reconcile %+v", rec)
	}
	requireBuckets(t, "nothing moved", e.buckets(p.mandate), r1Reserved)
	if g := a.gate(); g.Passed || gateCheck(g, "ALL_OPERATIONS_PRESENT").Passed {
		t.Fatalf("gate %+v", g)
	}
}

// Both restored after a committed order whose CREATE_DONE survived: one
// reconciliation pass rebuilds the operation, commits it from CREATE_DONE,
// and, because the order is gone from Odoo, writes its BLOCK and flags it
// (case E). The gate stays closed until Uday records the finding.
func TestD5BothRestoredAfterCommitWithCreateDone(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	snap := e.snapshotEngine()
	destBefore := a.s.Backup()
	mustDispatch(t, e, p)
	requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	a.s.Restore(destBefore)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Recreated) != 1 || len(rec.Flagged) != 1 || len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if e.dispatchStatus(p.operation) != engine.DispatchCommitted {
		t.Fatalf("status %s", e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "committed, no refund", e.buckets(p.mandate), r1Committed)
	if g := a.gate(); g.Passed || gateCheck(g, "HELD_RULE").Passed {
		t.Fatalf("gate passed without the finding: %+v", g)
	}
	if _, err := e.eng.RecordHeldFinding(context.Background(), uday, p.operation, "order erased when both stores were restored", "restore-log"); err != nil {
		t.Fatal(err)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	if r := a.s.Deliver(stubActionFor(t, e, p)); !r.Outcome.Tombstones() || a.s.Creates() != 1 {
		t.Fatalf("retry: %s, creates %d", r.Outcome, a.s.Creates())
	}
}
