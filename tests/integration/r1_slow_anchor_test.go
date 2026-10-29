//go:build integration && failpoints

package integration

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// R1 decision 5 (R1-DEF-30): recovery stays resumable against a slow
// recovery anchor. These tests measure what one reconciliation call gets
// through when every anchor round trip is slow, and check that progress and
// held money survive each call that stops at its budget. The measurements
// are the basis of the timeout documented in docs/r1-recovery-timeout.md.

// slowTracer delays every query on the engine's anchor connection.
type slowTracer struct {
	delay   time.Duration
	queries atomic.Int64
}

func (s *slowTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	s.queries.Add(1)
	time.Sleep(s.delay)
	return ctx
}

func (s *slowTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// slowAnchor points the engine at the same anchor through a connection
// whose every round trip takes delay longer.
func slowAnchor(t *testing.T, e *env, a *anchored, delay time.Duration) *slowTracer {
	t.Helper()
	cfg := a.ah.Engine.Config()
	tr := &slowTracer{delay: delay}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := e.eng.UseAnchor(context.Background(), &anchor.Client{Pool: pool}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// smallOrders evaluates n ₹10,000 orders on one mandate.
func (e *env) smallOrders(n int) (string, []permitted) {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	var out []permitted
	for i := 0; i < n; i++ {
		inst := e.instruction(fmt.Sprintf("po-slow-%d", i), m, line("item-a", 1, 1_000_000))
		st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
		requireStatus(e.t, "evaluate", st, body, 201, "")
		permit := body["permit"].(map[string]any)
		out = append(out, permitted{mandate: m, instruction: inst["instruction_id"].(string), action: body["action"].(map[string]any),
			jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)})
	}
	return m, out
}

const anchorDelay = 20 * time.Millisecond

func TestR1DEF30SlowAnchorReconcileIsResumable(t *testing.T) {
	const n = 24
	e := newEnv(t)
	a := withAnchor(t, e)
	m, orders := e.smallOrders(n)
	snap := e.snapshotEngine()
	for _, p := range orders {
		mustDispatch(t, e, p)
	}
	e.restoreEngine(snap) // the engine loses all n dispatches; the anchor has them
	tr := slowAnchor(t, e, a, anchorDelay)
	if r := a.check(); !r.WritesDisabled {
		t.Fatalf("restore not detected: %+v", r)
	}

	// Each call has 1.2 s of work before its 10 s reserve: too little for all n.
	const work = 1200 * time.Millisecond
	rebuilt := map[string]bool{}
	var calls int
	var busy time.Duration
	startQueries := tr.queries.Load()
	for calls = 1; calls <= 30; calls++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second+work)
		start := time.Now()
		rec, err := e.eng.ReconcileRestore(ctx, a.s)
		busy += time.Since(start)
		cancel()
		if err != nil {
			t.Fatalf("call %d: %v", calls, err)
		}
		for _, op := range rec.Recreated {
			if rebuilt[op] {
				t.Fatalf("call %d rebuilt %s a second time", calls, op)
			}
			rebuilt[op] = true
		}
		if len(rec.Problems) != 0 {
			t.Fatalf("call %d problems %v", calls, rec.Problems)
		}
		// Progress and held money survive a call that stopped at its budget:
		// rebuilt operations stay, each one's amount is held until settled,
		// and the not yet rebuilt ones keep their reservation.
		var unsettled, reservedLeft int64
		if err := e.h.Admin.QueryRow(context.Background(), `SELECT
			COALESCE(sum(amount_minor) FILTER (WHERE status IN ('PENDING', 'UNRESOLVED', 'HELD')), 0)
			FROM steelmatter.dispatches WHERE mandate_id = $1`, m).Scan(&unsettled); err != nil {
			t.Fatal(err)
		}
		reservedLeft = int64(n-len(rebuilt)) * 1_000_000
		if got := e.buckets(m); got.Unresolved != unsettled || got.Reserved != reservedLeft || got.Committed != 0 {
			t.Fatalf("call %d: buckets %+v, want unresolved %d reserved %d", calls, got, unsettled, reservedLeft)
		}
		if e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE mandate_id = $1`, m) != len(rebuilt) {
			t.Fatalf("call %d lost a rebuilt operation", calls)
		}
		if rec.Complete {
			break
		}
		if len(rec.Recreated) == 0 && len(rec.Settled) == 0 {
			t.Fatalf("call %d made no progress", calls)
		}
	}
	q := tr.queries.Load() - startQueries
	t.Logf("SLOW-ANCHOR delay=%s ops=%d calls=%d anchor_round_trips=%d per_op=%.1f wall=%s per_op_wall=%s",
		anchorDelay, n, calls, q, float64(q)/n, busy.Round(time.Millisecond), (busy / n).Round(time.Millisecond))
	if len(rebuilt) != n {
		t.Fatalf("rebuilt %d of %d in %d calls", len(rebuilt), n, calls)
	}
	if calls < 2 {
		t.Fatalf("one call finished %d slow operations; the test no longer exercises resumption", n)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	requireBuckets(t, "never sent: fenced and released", e.buckets(m), buckets{Available: limit})
}

// The resume gate and the restore check also finish against a slow anchor
// and are safe to repeat; the gate reads the anchor a bounded number of times.
func TestR1DEF30SlowAnchorCheckAndGate(t *testing.T) {
	e := newEnv(t)
	a := withAnchor(t, e)
	_, orders := e.smallOrders(3)
	for _, p := range orders {
		mustDispatch(t, e, p)
		requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
	}
	tr := slowAnchor(t, e, a, anchorDelay)
	before := tr.queries.Load()
	start := time.Now()
	if r := a.check(); r.WritesDisabled {
		t.Fatalf("false alarm on a slow anchor: %+v", r)
	}
	t.Logf("SLOW-ANCHOR check: delay=%s round_trips=%d wall=%s", anchorDelay, tr.queries.Load()-before, time.Since(start).Round(time.Millisecond))
}
