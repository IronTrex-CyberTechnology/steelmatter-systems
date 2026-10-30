//go:build integration && failpoints

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// Regression tests for the engine-side R1 defects found by SDET on
// 2ec2d8a (R1 Detailed Defect Report, 29 Oct 2026). Each test is named after
// its defect ID and reproduces the reported scenario.

// forgedReply rewrites every execute and fence reply into "fenced, block
// committed, no earlier create": a compromised connector or a manipulated
// reply claiming the destination holds no order.
type forgedReply struct{ connector.Connector }

func (f forgedReply) forge(r connector.Result) connector.Result {
	yes, no := true, false
	r.Outcome, r.BlockCommitted, r.PriorCreate, r.CreateDone, r.Destination = connector.Fenced, &yes, &no, false, nil
	return r
}

func (f forgedReply) Execute(ctx context.Context, a connector.Action) (connector.Result, error) {
	r, err := f.Connector.Execute(ctx, a)
	return f.forge(r), err
}

func (f forgedReply) Fence(ctx context.Context, opRef string, minSeq int64) (connector.Result, error) {
	r, err := f.Connector.Fence(ctx, opRef, minSeq)
	return f.forge(r), err
}

// claimsFenced never reaches the destination; it only claims a fence.
type claimsFenced struct{ connector.Connector }

func (c claimsFenced) Execute(_ context.Context, a connector.Action) (connector.Result, error) {
	return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "TEST_NOT_SENT"}, nil
}

func (c claimsFenced) Fence(_ context.Context, opRef string, _ int64) (connector.Result, error) {
	yes, no := true, false
	return connector.Result{OpRef: opRef, Call: "fence", Outcome: connector.Fenced, BlockCommitted: &yes, PriorCreate: &no}, nil
}

// R1-DEF-09: a reply alone never releases held money. The order exists, the
// reply claims a clean fence; the anchor's own destination stream records
// the create attempt, so the operation is HELD and the amount stays held.
func TestR1DEF09ForgedFenceOverExistingOrderIsHeld(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	f := forgedReply{a.s}
	r := e.settle(p.operation, f)
	if r.Execution == engine.DispatchFenced || a.s.Creates() != 1 {
		t.Fatalf("forged reply released money over a real order: %s (%d orders)", r.Execution, a.s.Creates())
	}
	requireExecution(t, "forged fence", r, engine.DispatchHeld)
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
}

// R1-DEF-09: a claimed fence the destination never recorded (no BLOCK in the
// anchor) is not proof either: the amount stays held and the fence is retried.
func TestR1DEF09ClaimedFenceWithoutAnchorBlockKeepsHold(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	c := claimsFenced{a.s}
	requireExecution(t, "not sent", e.settle(p.operation, c), engine.DispatchUnresolved)
	requireExecution(t, "claimed fence", e.settle(p.operation, c), engine.DispatchUnresolved)
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
	// The real destination then fences: BLOCK is recorded and money is released.
	requireExecution(t, "real fence", e.settle(p.operation, a.s), engine.DispatchFenced)
	requireBuckets(t, "released on proof", e.buckets(p.mandate), r1Released)
}

// switchOffFirst turns engine writes off at the moment the create is sent:
// the worst case of a settle that checked the write state just before.
type switchOffFirst struct {
	connector.Connector
	c *anchor.Client
}

func (s switchOffFirst) Execute(ctx context.Context, a connector.Action) (connector.Result, error) {
	if _, err := s.c.DisableWrites(ctx, "test: restore detected while a create was in flight"); err != nil {
		return connector.Result{}, err
	}
	return s.Connector.Execute(ctx, a)
}

// R1-DEF-14: once writes are off, no new create starts at the destination,
// even for a send already past the engine's own check. The anchor refuses
// the module's CREATE_INTENT, the destination fails closed, and the
// operation is fenced and released later.
func TestR1DEF14NoCreateAfterWritesOff(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	requireExecution(t, "send refused", e.settle(p.operation, switchOffFirst{a.s, a.c}), engine.DispatchUnresolved)
	if a.s.Creates() != 0 {
		t.Fatalf("an order was created after writes were turned off (%d)", a.s.Creates())
	}
	if n := a.count(anchor.Outcome); n == 0 {
		t.Fatal("outcome not recorded")
	}
	var intents int
	if err := a.ah.Admin.QueryRow(context.Background(), `SELECT count(*) FROM anchor.destination_records WHERE record_type = 'CREATE_INTENT'`).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Fatalf("CREATE_INTENT appended after writes off (%d)", intents)
	}
	requireExecution(t, "fenced while writes off", e.settle(p.operation, a.s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
}

// R1-DEF-12: with no connector configured, dispatch is refused with 503 and
// nothing is consumed or moved.
func TestR1DEF12DispatchWithoutConnectorChangesNothing(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	e.eng.Connector = nil
	st, body := e.call("POST", "/v1/dispatches", "agent-a", dispatchBody(p))
	requireStatus(t, "dispatch without connector", st, body, 503, "SERVICE_UNAVAILABLE")
	requireBuckets(t, "nothing moved", e.buckets(p.mandate), r1Reserved)
	if n := e.count(`SELECT count(*) FROM steelmatter.dispatches`); n != 0 {
		t.Fatalf("dispatches %d", n)
	}
	if status, _ := e.issuanceStatus(p.permitID); status != "ACTIVE" {
		t.Fatalf("permit %s", status)
	}
}

// failingTransport fails every call with a transport error.
type failingTransport struct{ connector.Connector }

func (failingTransport) Execute(context.Context, connector.Action) (connector.Result, error) {
	return connector.Result{}, errors.New("dial tcp 127.0.0.1:1: connection refused")
}

// R1-DEF-15: an unreachable anchor or destination is a retryable 503 on the
// recovery endpoints and on settle, never a non-retryable 500.
func TestR1DEF15DependencyOutagesAre503(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	e.httpOperators()
	p := e.permitted()
	mustDispatch(t, e, p)

	e.eng.Connector = failingTransport{a.s}
	st, body := e.call("POST", "/v1/dispatches/"+p.operation+"/settle", "uday", nil)
	requireStatus(t, "settle, destination down", st, body, 503, "SERVICE_UNAVAILABLE")
	if body["retryable"] != true {
		t.Fatalf("not retryable: %v", body)
	}
	e.eng.Connector = a.s

	dead, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dead.Close)
	e.eng.Anchor = &anchor.Client{Pool: dead}
	for _, path := range []string{"/v1/recovery/check", "/v1/recovery/reconcile", "/v1/recovery/resume"} {
		st, body := e.call("POST", path, "uday", nil)
		requireStatus(t, path+", anchor down", st, body, 503, "SERVICE_UNAVAILABLE")
		if body["retryable"] != true {
			t.Fatalf("%s not retryable: %v", path, body)
		}
	}
	requireBuckets(t, "nothing moved", e.buckets(p.mandate), r1Unresolved)
}

// R1-API-06: a principal who may not call an operator endpoint gets 403,
// whatever the service configuration; 503 is only for operators.
func TestR1API06RoleIsCheckedBeforeConfiguration(t *testing.T) {
	e := newR1Env(t)
	e.httpOperators()
	for _, c := range []struct {
		path string
		body any
	}{
		{"/v1/dispatches/op-1/findings", map[string]any{"finding": "f", "evidence_ref": "e"}},
		{"/v1/dispatches/op-1/repairs", map[string]any{"outcome": "COMMIT", "evidence_ref": "e"}},
		{"/v1/dispatches/op-1/repairs/rep-1/review", map[string]any{"approved": true}},
		{"/v1/dispatches/op-1/repairs/rep-1/confirm", nil},
		{"/v1/dispatches/op-1/anchor-repair", map[string]any{"repair_id": "rep-1"}},
	} {
		for _, who := range []string{"agent-a", "observer-a", "owner-a"} {
			st, body := e.call("POST", c.path, who, c.body)
			requireStatus(t, who+" "+c.path, st, body, 403, "FORBIDDEN")
		}
		st, body := e.call("POST", c.path, "uday", c.body)
		requireStatus(t, "operator "+c.path, st, body, 503, "SERVICE_UNAVAILABLE")
	}
}

// R1-DEF-06: a dispatch whose engine commit fails after its anchor intent
// is not a restore. The restore check reports it as unconfirmed and keeps
// writes on; other purchases on the same mandate continue; the same
// dispatch can be retried and commits once.
func TestR1DEF06FailedDispatchCommitIsNotARestore(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	failpoint.Set(failpoint.DispatchBeforeCommit, func(context.Context) error { return failpoint.ErrInjected })
	t.Cleanup(failpoint.Clear)
	if _, err := e.dispatch("agent-a", p); err == nil {
		t.Fatal("dispatch should fail at the crash point")
	}
	failpoint.Clear()
	if a.count(anchor.DispatchIntent) != 1 {
		t.Fatal("the failed attempt left no intent; the scenario needs one")
	}
	r := a.check()
	if r.WritesDisabled || len(r.Signals) != 0 || !a.writes().Enabled {
		t.Fatalf("a failed dispatch commit was taken for a restore: %+v", r)
	}
	if len(r.Unconfirmed) != 1 || r.Unconfirmed[0] != p.operation {
		t.Fatalf("unconfirmed %v", r.Unconfirmed)
	}
	requireBuckets(t, "nothing consumed", e.buckets(p.mandate), r1Reserved)

	// Another purchase on the same mandate is admitted (no admission barrier).
	inst := e.instruction("po-request-other", p.mandate, line("chair", 1, 100))
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "unrelated evaluation", st, body, 201, "")

	// The client retries the same dispatch: the intent append is an
	// idempotent repeat and the dispatch commits once.
	mustDispatch(t, e, p)
	var n int
	if err := a.ah.Admin.QueryRow(context.Background(), `SELECT count(*) FROM anchor.engine_records WHERE record_type = 'DISPATCH_INTENT' AND op_ref = $1`,
		p.operation).Scan(&n); err != nil || n != 1 {
		t.Fatalf("intents for %s: %d (%v)", p.operation, n, err)
	}
	requireExecution(t, "settle after retry", e.settle(p.operation, a.s), engine.DispatchCommitted)
	if r := a.check(); len(r.Signals) != 0 || len(r.Unconfirmed) != 0 {
		t.Fatalf("after the retry: %+v", r)
	}
	a.verify()
}

// R1-DEF-06: reconciliation never consumes or fences an unconfirmed
// dispatch's permit; the gate does not count it as a missing operation.
func TestR1DEF06ReconcileLeavesUnconfirmedDispatchAlone(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	failpoint.Set(failpoint.DispatchBeforeCommit, func(context.Context) error { return failpoint.ErrInjected })
	t.Cleanup(failpoint.Clear)
	if _, err := e.dispatch("agent-a", p); err == nil {
		t.Fatal("dispatch should fail at the crash point")
	}
	failpoint.Clear()
	if _, err := a.c.DisableWrites(context.Background(), "test: operator maintenance"); err != nil {
		t.Fatal(err)
	}
	rec := a.reconcile()
	if len(rec.Recreated) != 0 || len(rec.Unconfirmed) != 1 || !rec.Complete || len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if st, _ := e.issuanceStatus(p.permitID); st != "ACTIVE" {
		t.Fatalf("permit %s", st)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	mustDispatch(t, e, p)
}

// R1-DEF-20: an order deleted in Odoo after it was committed is flagged on
// its own operation; it stays committed (no refund), writes stay on, other
// purchases continue, and a HELD finding can be recorded.
func TestR1DEF20DeletedOrderIsFlaggedWithoutOutage(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
	if !a.s.DeleteOrder(p.operation) {
		t.Fatal("stub could not delete the order")
	}
	r := a.check()
	if r.WritesDisabled || len(r.Signals) != 0 || !a.writes().Enabled {
		t.Fatalf("a deleted order shut down writes: %+v", r)
	}
	if len(r.Flagged) != 1 || r.Flagged[0] != p.operation {
		t.Fatalf("flagged %v", r.Flagged)
	}
	var flag string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT COALESCE(flag, '') FROM steelmatter.dispatches WHERE op_ref = $1`, p.operation).Scan(&flag); err != nil ||
		flag != engine.FlagOrderDeletedAtDestination {
		t.Fatalf("flag %q (%v)", flag, err)
	}
	requireExecution(t, "still committed", e.settle(p.operation, a.s), engine.DispatchCommitted)
	requireBuckets(t, "no refund", e.buckets(p.mandate), r1Committed)
	if _, err := e.eng.RecordHeldFinding(context.Background(), uday, p.operation, "order deleted in Odoo by an administrator", "odoo-audit-log"); err != nil {
		t.Fatalf("finding: %v", err)
	}
	if r := a.check(); len(r.Flagged) != 0 || len(r.Signals) != 0 {
		t.Fatalf("second check: %+v", r)
	}
	// A late create for the deleted op_ref never creates a second order.
	if res := a.s.Deliver(stubActionFor(t, e, p)); res.Outcome == connector.Created || a.s.Creates() != 1 {
		t.Fatalf("re-create after deletion: %s (%d orders)", res.Outcome, a.s.Creates())
	}
	// Other purchasing continues.
	inst := e.instruction("po-request-after-delete", p.mandate, line("chair", 1, 100))
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "unrelated evaluation", st, body, 201, "")
	a.verify()
}

// R1-DEF-30: a reconciliation that reaches its time budget stops between
// operations, says it is incomplete, and the next run finishes the work.
func TestR1DEF30ReconcileStopsAtItsBudgetAndResumes(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	snap := e.snapshotEngine()
	mustDispatch(t, e, p)
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	short, cancel := context.WithTimeout(context.Background(), 2*time.Second) // inside the reserve
	defer cancel()
	rec, err := e.eng.ReconcileRestore(short, a.s)
	if err != nil || rec.Complete || len(rec.Recreated) != 0 {
		t.Fatalf("budgeted reconcile: %+v %v", rec, err)
	}
	rec = a.reconcile()
	if !rec.Complete || len(rec.Recreated) != 1 {
		t.Fatalf("second reconcile %+v", rec)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
}

// R1-DEF-05: an engine restored to before the instruction and permit
// existed is rebuilt from the anchor's signed intent. Reconciliation
// restores the instruction, action revision and permit, holds the amount,
// confirms the order at the destination without creating another, and the
// gate resumes writes; new purchases work afterwards.
func TestR1DEF05RestoreBeforePermitIsRebuiltFromAnchor(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	s := a.s
	m := e.mandate(mandateOpts{limit: 2 * limit}) // room for a second purchase
	e.assign(m, "agent-a")
	snap := e.snapshotEngine() // before the instruction and permit exist
	p := e.permittedFor(m, "po-request-001")
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, s), engine.DispatchCommitted)
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Problems) != 0 || len(rec.Recreated) != 1 || rec.Recreated[0] != p.operation {
		t.Fatalf("reconcile %+v", rec)
	}
	d, err := e.eng.GetDispatch(context.Background(), tenant, p.operation)
	if err != nil {
		t.Fatal(err)
	}
	requireExecution(t, "rebuilt", d, engine.DispatchCommitted)
	requireBuckets(t, "rebuilt", e.buckets(m), buckets{Available: 2*limit - sixLakh, Committed: sixLakh})
	if st, _ := e.issuanceStatus(p.permitID); st != "CONSUMED" {
		t.Fatalf("issuance %s", st)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE instruction_id = '` + p.instruction + `'`); n != 1 {
		t.Fatalf("instruction rows %d", n)
	}
	if g := a.gate(); !g.Passed || !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	if s.Creates() != 1 {
		t.Fatalf("recovery created another order: creates %d", s.Creates())
	}
	// Writes are back: a new purchase dispatches and commits; the rebuilt
	// source record cannot be reused for a second instruction.
	q := e.permittedFor(m, "po-request-002")
	mustDispatch(t, e, q)
	requireExecution(t, "new purchase", e.settle(q.operation, s), engine.DispatchCommitted)
	if s.Creates() != 2 {
		t.Fatalf("creates %d", s.Creates())
	}
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m, line("item-a", 2, 1_000_000)))
	if st == 201 {
		t.Fatalf("source record reused: %d %v", st, body)
	}
}

// R1-OBS-01 / R1-DEF-05: what cannot be rebuilt is named, not an internal
// error, and the gate stays closed. Here the engine was restored to before
// the mandate existed.
func TestR1OBS01MissingMandateIsNamed(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	snap := e.snapshotEngine() // before the mandate exists
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	p := e.permittedFor(m, "po-request-001")
	mustDispatch(t, e, p)
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Problems) != 1 || !strings.Contains(rec.Problems[0], "MANDATE_MISSING") || strings.Contains(rec.Problems[0], "INTERNAL_ERROR") {
		t.Fatalf("problems %v", rec.Problems)
	}
	if g := a.gate(); g.Passed {
		t.Fatalf("gate passed with a missing operation: %+v", g)
	}
}

func (e *env) permittedFor(m, source string) permitted {
	e.t.Helper()
	inst := e.instruction(source, m)
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(e.t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	return permitted{mandate: m, instruction: inst["instruction_id"].(string), action: body["action"].(map[string]any),
		jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
}

// R1-P4-DEF-01: concurrent reconciliations and resumes report only their
// own work: one rebuild, one settle, one resume.
func TestR1P4DEF01ConcurrentRecoveryReportsOwnWork(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	snap := e.snapshotEngine()
	mustDispatch(t, e, p)
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	var wg sync.WaitGroup
	reps := make([]*engine.ReconcileReport, 2)
	for i := range reps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.eng.ReconcileRestore(context.Background(), a.s)
			if err != nil {
				t.Error(err)
				return
			}
			reps[i] = r
		}(i)
	}
	wg.Wait()
	if reps[0] == nil || reps[1] == nil {
		t.FailNow()
	}
	if n := len(reps[0].Recreated) + len(reps[1].Recreated); n != 1 {
		t.Fatalf("recreated reported %d times: %+v %+v", n, reps[0], reps[1])
	}
	if n := len(reps[0].Settled) + len(reps[1].Settled); n != 1 {
		t.Fatalf("settled reported %d times: %+v %+v", n, reps[0], reps[1])
	}
	gates := make([]*engine.GateReport, 2)
	for i := range gates {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, err := e.eng.TryResume(context.Background(), a.s)
			if err != nil {
				t.Error(err)
				return
			}
			gates[i] = g
		}(i)
	}
	wg.Wait()
	if gates[0] == nil || gates[1] == nil {
		t.FailNow()
	}
	if gates[0].Resumed == gates[1].Resumed {
		t.Fatalf("resumed %v and %v; want exactly one", gates[0].Resumed, gates[1].Resumed)
	}
}

// R1-DEF-03: the destination is the supplier revision source. A supplier
// active at the destination needs no manual engine row; after a change at
// the destination the old permit is refused and a fresh authorisation uses
// the new revision and commits; a revision written backwards at the
// destination never makes the engine forget the newer one.
func TestR1DEF03SupplierRevisionComesFromTheDestination(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	s.SetSupplierRevision("supplier-x", 3)
	m := e.mandate(mandateOpts{suppliers: []string{"supplier-x"}})
	e.assign(m, "agent-a")
	body := instructionBody("po-x-1", m, line("item-a", 1, 1_000_000))
	body["proposed_action"].(map[string]any)["supplier_id"] = "supplier-x"
	st, inst := e.call("POST", "/v1/instructions", "issuer-a", body)
	requireStatus(t, "instruction for a supplier only the destination knows", st, inst, 201, "")
	revision := func() int64 {
		var r int64
		if err := e.h.Admin.QueryRow(context.Background(), `SELECT revision FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = 'supplier-x'`, tenant).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := revision(); r != 3 {
		t.Fatalf("engine revision %d", r)
	}
	st, ev := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate", st, ev, 201, "")
	old := permitted{mandate: m, instruction: inst["instruction_id"].(string), action: ev["action"].(map[string]any), jws: ev["permit_jws"].(string)}

	s.SetSupplierRevision("supplier-x", 4) // routine change at the destination
	if _, err := e.dispatch("agent-a", old); err == nil {
		t.Fatal("a permit for the old supplier revision was dispatched")
	}
	body = instructionBody("po-x-2", m, line("item-a", 1, 1_000_000))
	body["proposed_action"].(map[string]any)["supplier_id"] = "supplier-x"
	st, inst2 := e.call("POST", "/v1/instructions", "issuer-a", body)
	requireStatus(t, "fresh instruction", st, inst2, 201, "")
	st, ev2 := e.evaluate("agent-a", inst2["instruction_id"].(string), 1)
	requireStatus(t, "fresh evaluate", st, ev2, 201, "")
	if rev := ev2["action"].(map[string]any)["supplier_revision"]; fmt.Sprint(rev) != "4" {
		t.Fatalf("fresh authorisation uses supplier revision %v", rev)
	}
	fresh := permitted{mandate: m, instruction: inst2["instruction_id"].(string), action: ev2["action"].(map[string]any), jws: ev2["permit_jws"].(string),
		operation: ev2["permit"].(map[string]any)["operation_id"].(string)}
	mustDispatch(t, e, fresh)
	requireExecution(t, "fresh authorisation commits", e.settle(fresh.operation, s), engine.DispatchCommitted)

	s.SetSupplierRevision("supplier-x", 1) // written backwards at the destination
	body = instructionBody("po-x-3", m, line("item-a", 1, 1_000_000))
	body["proposed_action"].(map[string]any)["supplier_id"] = "supplier-x"
	st, _ = e.call("POST", "/v1/instructions", "issuer-a", body)
	requireStatus(t, "instruction after a backwards write", st, nil, 201, "")
	if r := revision(); r != 4 {
		t.Fatalf("a backwards write lowered the engine revision to %d", r)
	}
}
