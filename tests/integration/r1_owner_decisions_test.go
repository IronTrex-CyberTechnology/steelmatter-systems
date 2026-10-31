//go:build integration && failpoints

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/dbtest"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/server"
)

// Aakhil's decisions of 30 Oct 2026 on the five SDET points (engine side).

// NEW-SDET-01: one active backend per deployment. A second backend for the
// same database refuses to start and changes nothing; the first keeps
// serving. Once the first stops, a backend can start again.
func TestR1SingleBackendPerDeployment(t *testing.T) {
	e := newEnv(t)
	first := e.start("")
	firstURL := e.url
	second := e.startWith("", "100")
	select {
	case err := <-second.done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 || !strings.Contains(second.out.String(), "another Steelmatter backend is already active") {
			t.Fatalf("second backend: %v\n%s", err, second.out)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("second backend kept running\n%s", second.out)
	}
	res, err := http.Get(firstURL + "/v1/ready")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || first.cmd.ProcessState != nil {
		t.Fatalf("first backend disturbed: ready %d\n%s", res.StatusCode, first.out)
	}
	_ = first.cmd.Process.Kill()
	<-first.done
	third := e.startWith("", "100")
	if third.cmd.ProcessState != nil || e.url != third.url {
		t.Fatalf("backend did not start after the first stopped\n%s", third.out)
	}
}

// The lock refuses a second holder in the anchor too and is released on
// close.
func TestR1InstanceLockInEngineAndAnchor(t *testing.T) {
	h := dbtest.New(t)
	ah := dbtest.NewAnchor(t)
	ctx := context.Background()
	pools := map[string]*pgxpool.Pool{"engine database": h.Executor, "recovery anchor": ah.Engine}
	in, err := engine.AcquireInstance(ctx, pools)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range pools {
		if _, err := engine.AcquireInstance(ctx, map[string]*pgxpool.Pool{name: p}); !errors.Is(err, engine.ErrAnotherInstance) {
			t.Fatalf("%s: second holder %v", name, err)
		}
	}
	in.Close()
	again, err := engine.AcquireInstance(ctx, pools)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again.Close()
}

// NEW-QA-03 (decision 7): the running backend never uses owner credentials.
func TestR1BackendRefusesOwnerCredentials(t *testing.T) {
	h := dbtest.New(t)
	ah := dbtest.NewAnchor(t)
	ctx := context.Background()
	if err := engine.CheckNotOwner(ctx, h.Admin, "engine", "steelmatter", "steelmatter_meta", "target"); err == nil {
		t.Fatal("engine owner credentials accepted")
	}
	if err := engine.CheckNotOwner(ctx, h.Executor, "engine", "steelmatter", "steelmatter_meta", "target"); err != nil {
		t.Fatalf("executor refused: %v", err)
	}
	if err := engine.CheckNotOwner(ctx, ah.Admin, "anchor", "anchor"); err == nil {
		t.Fatal("anchor owner credentials accepted")
	}
	if err := engine.CheckNotOwner(ctx, ah.Engine, "anchor", "anchor"); err != nil {
		t.Fatalf("anchor engine role refused: %v", err)
	}
}

// Clocks: the 2-second limit applies to the recovery anchor. A larger
// difference fails readiness and new execution and turns writes off; an
// unreadable anchor clock fails readiness; writes return only through the
// resume gate once the clocks agree and recovery is verified.
func TestR1AnchorClockLimit(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	ctx := context.Background()
	skew := func(d time.Duration) {
		e.eng.AnchorClock = func(ctx context.Context) (time.Time, error) {
			t, err := a.c.Now(ctx)
			return t.Add(d), err
		}
	}
	skew(-1500 * time.Millisecond)
	if err := e.eng.Ready(ctx); err != nil {
		t.Fatalf("1.5 s is within the limit: %v", err)
	}

	p := e.permitted()
	skew(-90 * time.Second)
	requireFault(t, "ready", e.eng.Ready(ctx), "CLOCK_UNHEALTHY", "differs")
	_, err := e.dispatch("agent-a", p)
	if err == nil {
		t.Fatal("dispatch with a skewed anchor clock")
	}
	if e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 || a.s.Creates() != 0 {
		t.Fatal("something was executed")
	}
	ws, err := a.c.WriteState(ctx)
	if err != nil || ws.Enabled || !strings.Contains(ws.Reason, engine.SignalClockSkew) {
		t.Fatalf("writes %+v %v", ws, err)
	}
	if r := a.check(); !hasSignal(r, engine.SignalClockSkew) {
		t.Fatalf("restore check without CLOCK_SKEW: %+v", r.Signals)
	}
	if g := a.gate(); g.Passed || !failedCheck(g, "CLOCKS_AGREE") {
		t.Fatalf("gate with skewed clocks: %+v", g)
	}

	e.eng.AnchorClock = func(context.Context) (time.Time, error) { return time.Time{}, errors.New("anchor clock unreadable") }
	requireFault(t, "ready, clock unreadable", e.eng.Ready(ctx), "CLOCK_UNHEALTHY", "cannot be checked")

	e.eng.AnchorClock = nil // clocks fixed
	if g := a.gate(); !g.Passed || !g.Resumed {
		t.Fatalf("gate after the clocks agree: %+v", g)
	}
	if err := e.eng.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	mustDispatch(t, e, p)
}

func failedCheck(g *engine.GateReport, name string) bool {
	for _, c := range g.Checks {
		if c.Name == name && !c.Passed {
			return true
		}
	}
	return false
}

// C-11 (T18): a mandate owner with independent approval rights may approve
// someone else's action; nobody approves an action they requested or
// execute (TestT18ApproverAuthority).
func TestR1OwnerMayApproveOthersAction(t *testing.T) {
	e := newR1Env(t)
	e.approvalPolicy()
	e.admin(`INSERT INTO steelmatter.approver_entitlements (tenant_id, approver_principal) VALUES ($1, 'owner-a')`, tenant)
	_, id, digest := e.openInstruction("po-c11-1")
	st, body := e.approve("owner-a", id, 1, digest, true)
	requireStatus(t, "owner approves the issuer's action", st, body, 201, "")
	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate", st, body, 201, "")
	for _, who := range []string{"issuer-a", "agent-a"} {
		_, id2, digest2 := e.openInstruction("po-c11-" + who)
		e.admin(`INSERT INTO steelmatter.approver_entitlements (tenant_id, approver_principal) VALUES ($1, $2) ON CONFLICT DO NOTHING`, tenant, who)
		st, body = e.approve(who, id2, 1, digest2, true)
		if st == 201 {
			t.Fatalf("%s approved an action they requested or execute: %v", who, body)
		}
	}
}

// NEW-QA-04: /v1/ready stays 503 when the backend is not ready, and names the
// actual reason instead of always blaming the authoritative database.
func TestR1ReadyNamesTheReason(t *testing.T) {
	e := newR1Env(t)
	h := server.New(server.Info{}, server.ReadyFunc(e.eng.Ready))
	ready := func(what, want string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ready", nil))
		if want == "" {
			if rec.Code != 200 {
				t.Fatalf("%s: %d %s", what, rec.Code, rec.Body)
			}
			return
		}
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"error_code":"SERVICE_UNAVAILABLE"`) || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: %d %s, want %q", what, rec.Code, rec.Body, want)
		}
	}
	ctx := context.Background()
	dead, err := pgxpool.New(ctx, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dead.Close)
	e.eng.Cfg.RequireRecoveryAnchor = true
	e.eng.Anchor = nil
	ready("anchor missing", "recovery anchor not configured")
	a := withAnchor(t, e)
	ready("healthy", "")
	e.eng.Anchor = &anchor.Client{Pool: dead}
	ready("anchor unavailable", "recovery anchor unavailable")
	e.eng.Anchor = a.c
	if _, err := a.c.DisableWrites(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	ready("writes disabled", "recovery in progress: writes disabled")
	live := e.eng.Pool
	e.eng.Pool = dead
	ready("database down", "authoritative database unavailable")
	e.eng.Pool = live
}
