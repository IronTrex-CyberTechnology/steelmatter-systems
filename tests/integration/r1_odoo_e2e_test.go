//go:build integration && failpoints && odoo

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// Engine plus real Odoo (R1 completion check: T33, T34, T35 traced to
// evidence). Runs only against Kaif's sandbox, through
// destinations/odoo/sandbox/test-engine, on a FRESH sandbox
// (sandbox/reset then sandbox/up): the sandbox's anchor is shared with the
// Odoo module, so records from an earlier run would look like a restore.
//
// The engine database is a disposable test database restored at database
// level; Odoo is restored with Kaif's sandbox helpers (a template copy of
// Odoo's database); the anchor is never restored.

const odooCompany = "acme-in" // the sandbox company's sm_ref and the module's STEELMATTER_DESTINATION

// lossy wraps the real connector to lose an execute reply after Odoo
// applied it (T33), or to lose the request before it reaches Odoo.
type lossy struct {
	connector.Connector
	mu      sync.Mutex
	drop    map[string]bool // execute applied in Odoo, reply lost
	notSent map[string]bool // execute never reaches Odoo
}

func (l *lossy) Execute(ctx context.Context, a connector.Action) (connector.Result, error) {
	l.mu.Lock()
	drop, notSent := l.drop[a.OpRef], l.notSent[a.OpRef]
	delete(l.drop, a.OpRef)
	delete(l.notSent, a.OpRef)
	l.mu.Unlock()
	if notSent {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "TEST_NOT_SENT"}, nil
	}
	r, err := l.Connector.Execute(ctx, a)
	if drop {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "TEST_REPLY_LOST"}, err
	}
	return r, err
}

func requireEnvVar(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set; run destinations/odoo/sandbox/test-engine", name)
	}
	return v
}

// sandboxOdoo runs one of Kaif's sandbox helpers (snapshot_odoo,
// restore_odoo) from destinations/odoo/tests/sandbox_client.py.
func sandboxOdoo(t *testing.T, fn string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	cmd := exec.Command("python3", "-I", "-c",
		"import sys; sys.path.insert(0, 'destinations/odoo/tests'); import sandbox_client as s; s."+fn+"()")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox %s: %v\n%s", fn, err, out)
	}
}

type odooRun struct {
	*anchored
	conn    *odoo.Connector
	l       *lossy
	mandate string
	n       int
}

func setupOdoo(t *testing.T) *odooRun {
	t.Helper()
	url := requireEnvVar(t, "STEELMATTER_ODOO_URL")
	anchorURL := requireEnvVar(t, "STEELMATTER_ANCHOR_URL")
	ctx := context.Background()
	conn, err := odoo.New(odoo.Config{URL: url, Database: requireEnvVar(t, "STEELMATTER_ODOO_DB"), Instance: "sandbox", Timeout: 60 * time.Second,
		Executor: odoo.Credentials{Login: "sm_executor", Password: requireEnvVar(t, "STEELMATTER_ODOO_EXECUTOR_PASSWORD")},
		Observer: odoo.Credentials{Login: "sm_observer", Password: requireEnvVar(t, "STEELMATTER_ODOO_OBSERVER_PASSWORD")}})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	pool, err := pgxpool.New(ctx, anchorURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c := &anchor.Client{Pool: pool}
	e.eng.Cfg.CompanyRef, e.eng.Cfg.RestoreGrace = odooCompany, 0
	if _, err := e.eng.UseAnchor(ctx, c); err != nil {
		t.Fatal(err)
	}
	if ws, _ := c.WriteState(ctx); !ws.Enabled {
		t.Fatalf("the sandbox anchor has writes off (%s); start from a fresh sandbox (reset, then up)", ws.Reason)
	}
	l := &lossy{Connector: conn, drop: map[string]bool{}, notSent: map[string]bool{}}
	e.eng.Connector = l
	e.eng.RepairReviewers, e.eng.RepairConfirmers = []string{"kaif"}, []string{"aakhil"}
	rev, err := conn.SupplierRevision(ctx, "sup-chairs")
	if err != nil {
		t.Fatal(err)
	}
	e.admin(`INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status) VALUES ($1, 'sup-chairs', $2, 'ACTIVE')`, tenant, rev)
	st, body := e.call("POST", "/v1/mandates", "owner-a", map[string]any{
		"actor_ids": []string{"agent-a"}, "audience": audience, "supplier_ids": []string{"sup-chairs"}, "delivery_ids": []string{"wh-chennai"},
		"currency": "INR", "limit_minor": limit, "expires_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z"),
	})
	requireStatus(t, "create mandate", st, body, 201, "")
	m := body["mandate"].(map[string]any)["mandate_id"].(string)
	e.assign(m, "agent-a")
	a := &anchored{e: e, c: c, s: nil}
	return &odooRun{anchored: a, conn: conn, l: l, mandate: m}
}

// order evaluates one ₹60,000 order (10 chairs at ₹6,000) on the mandate.
func (r *odooRun) order(t *testing.T) permitted {
	t.Helper()
	r.n++
	st, inst := r.e.call("POST", "/v1/instructions", "issuer-a", map[string]any{"source_system": source,
		"source_record_id": fmt.Sprintf("odoo-e2e-%d-%d", time.Now().UnixNano(), r.n), "mandate_id": r.mandate,
		"proposed_action": map[string]any{"supplier_id": "sup-chairs", "delivery_id": "wh-chennai", "currency": "INR",
			"lines": []map[string]any{line("chair-ergo", 10, 600000)}}})
	requireStatus(t, "create instruction", st, inst, 201, "")
	id := inst["instruction_id"].(string)
	st, body := r.e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	return permitted{mandate: r.mandate, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
}

func (r *odooRun) settle(t *testing.T, op string) *engine.DispatchResult {
	t.Helper()
	d, err := r.e.eng.Settle(context.Background(), tenant, op, r.e.eng.Connector)
	if err != nil {
		t.Fatalf("settle %s: %v", op, err)
	}
	return d
}

func (r *odooRun) check(t *testing.T) *engine.RestoreReport {
	t.Helper()
	rep, err := r.e.eng.CheckRestore(context.Background(), r.e.eng.Connector)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r *odooRun) reconcile(t *testing.T) *engine.ReconcileReport {
	t.Helper()
	rep, err := r.e.eng.ReconcileRestore(context.Background(), r.e.eng.Connector)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r *odooRun) resume(t *testing.T) {
	t.Helper()
	g, err := r.e.eng.TryResume(context.Background(), r.e.eng.Connector)
	if err != nil || !g.Resumed {
		b, _ := json.MarshalIndent(g, "", "  ")
		t.Fatalf("resume gate: %v\n%s", err, b)
	}
}

// evidence logs the anchor records of one operation, for the T33-T35 trace.
func (r *odooRun) evidence(t *testing.T, op string) {
	t.Helper()
	recs, err := r.c.RecordsFor(context.Background(), tenant, op)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		t.Logf("evidence %s anchor#%d %s %s", op, rec.Seq, rec.Type, rec.Payload)
	}
	if err := anchor.Verify(context.Background(), r.c.Pool); err != nil {
		t.Fatalf("anchor chain: %v", err)
	}
}

func TestR1OdooEndToEnd(t *testing.T) {
	r := setupOdoo(t)
	ctx := context.Background()

	t.Run("T33 reply lost after Odoo created the order: held, then found and committed once", func(t *testing.T) {
		p := r.order(t)
		mustDispatch(t, r.e, p)
		r.l.drop[p.operation] = true
		requireExecution(t, "lost reply", r.settle(t, p.operation), engine.DispatchUnresolved)
		requireBuckets(t, "held", r.e.buckets(p.mandate), buckets{Available: limit - 6_000_000, Unresolved: 6_000_000})
		requireExecution(t, "found", r.settle(t, p.operation), engine.DispatchCommitted)
		if lk, _ := r.conn.Lookup(ctx, p.operation); lk.Outcome != connector.Found {
			t.Fatalf("lookup %s", lk.Outcome)
		}
		r.evidence(t, p.operation)
	})

	t.Run("never reached Odoo: fenced and released; a late create is refused", func(t *testing.T) {
		p := r.order(t)
		mustDispatch(t, r.e, p)
		r.l.notSent[p.operation] = true
		requireExecution(t, "not sent", r.settle(t, p.operation), engine.DispatchUnresolved)
		requireExecution(t, "fenced", r.settle(t, p.operation), engine.DispatchFenced)
		if late, _ := r.conn.Execute(ctx, r.e.destActionForPermit(p)); late.Outcome != connector.RefusedFenced {
			t.Fatalf("late create %s", late.Outcome)
		}
		r.evidence(t, p.operation)
	})

	t.Run("T35 case A: engine restored before a confirmed purchase", func(t *testing.T) {
		p := r.order(t)
		snap := r.e.snapshotEngine()
		mustDispatch(t, r.e, p)
		requireExecution(t, "commit", r.settle(t, p.operation), engine.DispatchCommitted)
		r.e.restoreEngine(snap)
		if rep := r.check(t); !rep.WritesDisabled || !hasSignal(rep, engine.SignalEngineMissingOperation) {
			t.Fatalf("signals %+v", rep.Signals)
		}
		if rec := r.reconcile(t); len(rec.Recreated) != 1 || len(rec.Problems) != 0 {
			t.Fatalf("reconcile %+v", rec)
		}
		requireExecution(t, "rebuilt", r.settle(t, p.operation), engine.DispatchCommitted)
		r.resume(t)
		r.evidence(t, p.operation)
	})

	t.Run("T35 case 2: Odoo restored after a fence; the delayed create is refused by BLOCK", func(t *testing.T) {
		p := r.order(t)
		mustDispatch(t, r.e, p)
		r.l.notSent[p.operation] = true
		r.settle(t, p.operation)
		sandboxOdoo(t, "snapshot_odoo")
		requireExecution(t, "fenced", r.settle(t, p.operation), engine.DispatchFenced)
		sandboxOdoo(t, "restore_odoo")
		if late, _ := r.conn.Execute(ctx, r.e.destActionForPermit(p)); late.Outcome != connector.RefusedFenced {
			t.Fatalf("delayed create after the restore: %s", late.Outcome)
		}
		if rep := r.check(t); !rep.WritesDisabled {
			t.Fatalf("Odoo restore not detected: %+v", rep.Signals)
		}
		if rec := r.reconcile(t); len(rec.Problems) != 0 {
			t.Fatalf("reconcile %+v", rec)
		}
		r.resume(t)
		r.evidence(t, p.operation)
	})

	t.Run("T34 case E: Odoo forgot a committed order; a retry never creates a second one", func(t *testing.T) {
		p := r.order(t)
		sandboxOdoo(t, "snapshot_odoo")
		mustDispatch(t, r.e, p)
		requireExecution(t, "commit", r.settle(t, p.operation), engine.DispatchCommitted)
		sandboxOdoo(t, "restore_odoo")
		if retry, _ := r.conn.Execute(ctx, r.e.destActionForPermit(p)); !retry.Outcome.Tombstones() {
			t.Fatalf("retry after Odoo forgot the order: %s", retry.Outcome)
		}
		if rep := r.check(t); !rep.WritesDisabled || !hasSignal(rep, engine.SignalCommittedOrderMissing) {
			t.Fatalf("signals %+v", rep.Signals)
		}
		if rec := r.reconcile(t); len(rec.Flagged) != 1 || len(rec.Problems) != 0 {
			t.Fatalf("reconcile %+v", rec)
		}
		if _, err := r.e.eng.RecordHeldFinding(ctx, uday, p.operation, "Odoo restore erased a committed order", "e2e"); err != nil {
			t.Fatal(err)
		}
		r.resume(t)
		requireExecution(t, "still committed", r.settle(t, p.operation), engine.DispatchCommitted)
		r.evidence(t, p.operation)
	})

	t.Run("T35 both restored after a commit: rebuilt, committed from CREATE_DONE, blocked and flagged", func(t *testing.T) {
		p := r.order(t)
		snap := r.e.snapshotEngine()
		sandboxOdoo(t, "snapshot_odoo")
		mustDispatch(t, r.e, p)
		requireExecution(t, "commit", r.settle(t, p.operation), engine.DispatchCommitted)
		r.e.restoreEngine(snap)
		sandboxOdoo(t, "restore_odoo")
		if rep := r.check(t); !rep.WritesDisabled {
			t.Fatal("restore not detected")
		}
		rec := r.reconcile(t)
		if len(rec.Recreated) != 1 || len(rec.Problems) != 0 {
			t.Fatalf("reconcile %+v", rec)
		}
		if _, err := r.e.eng.RecordHeldFinding(ctx, uday, p.operation, "both stores restored after a committed order", "e2e"); err != nil {
			t.Fatal(err)
		}
		r.resume(t)
		r.evidence(t, p.operation)
	})

	t.Run("case C: an order only Odoo knows is rebuilt from the engine's permit", func(t *testing.T) {
		p := r.order(t)
		if cr, _ := r.conn.Execute(ctx, r.e.destActionForPermit(p)); cr.Outcome != connector.Created {
			t.Fatalf("order straight into Odoo: %s", cr.Outcome)
		}
		if rep := r.check(t); !rep.WritesDisabled || !hasSignal(rep, engine.SignalUnknownDestinationOp) {
			t.Fatalf("signals %+v", rep.Signals)
		}
		if rec := r.reconcile(t); len(rec.FromOdoo) != 1 || len(rec.Problems) != 0 {
			t.Fatalf("reconcile %+v", rec)
		}
		if err := r.e.eng.ConfirmAnchorRepair(ctx, aakhil, p.operation, "e2e"); err != nil {
			t.Fatal(err)
		}
		r.resume(t)
		r.evidence(t, p.operation)
	})
}
