//go:build integration && failpoints

package integration

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/auth"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/engine"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/failpoint"
)

// pauseAt makes the first request reaching name block (holding its locks)
// until release is called.
func pauseAt(t *testing.T, name string) (held <-chan struct{}, release func()) {
	t.Helper()
	h, r := make(chan struct{}), make(chan struct{})
	var once, relOnce sync.Once
	failpoint.Set(name, func(context.Context) error {
		first := false
		once.Do(func() { first = true })
		if first {
			close(h)
			<-r
		}
		return nil
	})
	rel := func() { relOnce.Do(func() { close(r) }) }
	t.Cleanup(func() { rel(); failpoint.Clear() })
	return h, rel
}

// waitLockWaiter blocks until some backend in the test database waits on a
// row lock. Ordering comes from the database state, not from sleeps.
func (e *env) waitLockWaiter() {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, e.h.DB.Name) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatal("no transaction ever blocked on a lock")
}

type result struct {
	status int
	body   map[string]any
}

func async(fn func() (int, map[string]any)) <-chan result {
	c := make(chan result, 1)
	go func() {
		st, b := fn()
		c <- result{st, b}
	}()
	return c
}

func wait(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(15 * time.Second):
		t.Fatal("pause point never reached")
	}
}

// ---- T07 revocation ordering ----

func TestT07RevocationBeforeAdmission(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	held, release := pauseAt(t, failpoint.RevokeAfterLock)
	revoke := async(func() (int, map[string]any) {
		return e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	})
	wait(t, held)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	e.waitLockWaiter()
	release()
	r, a := <-revoke, <-admit
	requireStatus(t, "revoke", r.status, r.body, 200, "")
	requireStatus(t, "admit", a.status, a.body, 403, "AUTHORITY_REVOKED")
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
	if s, reason := e.issuanceStatus(p.permitID); s != "RELEASED" || reason != "REVOKED" {
		t.Fatalf("issuance %s/%s", s, reason)
	}
}

func TestT07AdmissionBeforeRevocation(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	held, release := pauseAt(t, failpoint.AdmissionAfterLocks)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	wait(t, held)
	revoke := async(func() (int, map[string]any) {
		return e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	})
	e.waitLockWaiter()
	release()
	a, r := <-admit, <-revoke
	requireStatus(t, "admit", a.status, a.body, 201, "")
	requireStatus(t, "revoke", r.status, r.body, 200, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger) // one historical effect, not undone
}

// ---- T08 expiry after lock wait ----

func TestT08ExpiryAfterLockWait(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 2 * time.Second})
	p := e.permitted()
	ctx := context.Background()
	holder, err := e.h.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE mandate_id = $1 FOR UPDATE`, p.mandate); err != nil {
		t.Fatal(err)
	}
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	e.waitLockWaiter()
	e.waitExpired(p.permitID) // wall-clock expiry while the admission is blocked
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	a := <-admit
	requireStatus(t, "admit after wait", a.status, a.body, 403, "PERMIT_EXPIRED")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
}

// ---- T10 changed replay and authorised amendment ----

func amendBody(id string, expected int64, lines ...map[string]any) map[string]any {
	return map[string]any{"amendment_id": id, "expected_action_revision": expected,
		"proposed_action": instructionBody("x", "x", lines...)["proposed_action"]}
}

func TestT10ChangedReplayAndAmendment(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	path := "/v1/instructions/" + p.instruction + "/revisions"

	// Changed replay through create and admit conflicts without mutation.
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", p.mandate, line("item-a", 4, 20_000_000)))
	requireStatus(t, "changed create", st, body, 409, "SOURCE_CONFLICT")
	changed := clone(p.action)
	changed["delivery_id"] = "warehouse-b"
	st, body = e.admit("agent-a", changed, p.jws)
	requireStatus(t, "changed admit", st, body, 409, "ACTION_CHANGED")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)

	// Only an entitled issuer can amend.
	for who, want := range map[string]int{"agent-a": 403, "owner-a": 403, "issuer-b": 404} {
		st, body := e.call("POST", path, who, amendBody("a1", 1, line("item-a", 2, 20_000_000)))
		code := map[int]string{403: "FORBIDDEN", 404: "NOT_FOUND"}[want]
		requireStatus(t, "amend as "+who, st, body, want, code) // unentitled issuer: same as absent (no existence oracle)
	}
	st, body = e.call("POST", path, "issuer-x", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	requireStatus(t, "other tenant", st, body, 404, "NOT_FOUND")
	st, body = e.call("POST", path, "issuer-a", amendBody("a1", 2, line("item-a", 2, 20_000_000)))
	requireStatus(t, "stale expected revision", st, body, 409, "REVISION_STALE")

	// Authorised amendment: invalidates the permit, releases once, preserves identity.
	st, first := e.call("POST", path, "issuer-a", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	requireStatus(t, "amend", st, first, 201, "")
	if first["action_revision"].(interface{ String() string }).String() != "2" || first["released_minor"].(interface{ String() string }).String() != "60000000" {
		t.Fatalf("amendment %v", first)
	}
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
	if s, r := e.issuanceStatus(p.permitID); s != "RELEASED" || r != "AMENDED" {
		t.Fatalf("old issuance %s/%s", s, r)
	}
	act := first["action"].(map[string]any)
	if act["actor_id"] != "agent-a" || act["instruction_id"] != p.instruction {
		t.Fatal("amendment changed actor or instruction")
	}
	var mandate, sourceRec string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT mandate_id, source_record_id FROM steelmatter.instructions WHERE instruction_id = $1`, p.instruction).Scan(&mandate, &sourceRec)
	if mandate != p.mandate || sourceRec != "po-request-001" {
		t.Fatal("amendment changed mandate or source")
	}
	if e.count(`SELECT count(*) FROM steelmatter.issuances WHERE instruction_id = $1 AND action_revision = 2`, p.instruction) != 0 {
		t.Fatal("new revision has a reservation before re-evaluation")
	}

	// Exact retry returns the recorded result; changed reuse conflicts; nothing released twice.
	st, again := e.call("POST", path, "issuer-a", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	requireStatus(t, "amend retry", st, again, 200, "")
	if again["action_digest"] != first["action_digest"] {
		t.Fatal("retry returned a different revision")
	}
	st, body = e.call("POST", path, "issuer-a", amendBody("a1", 1, line("item-a", 1, 20_000_000)))
	requireStatus(t, "changed amendment id reuse", st, body, 409, "AMENDMENT_ID_CONFLICT")
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})

	// The old permit can no longer be used; the new revision needs evaluation.
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "old permit", st, body, 409, "REVISION_STALE")
	st, body = e.evaluate("agent-a", p.instruction, 1)
	requireStatus(t, "evaluate old revision", st, body, 409, "REVISION_STALE")
	st, ev := e.evaluate("agent-a", p.instruction, 2)
	requireStatus(t, "evaluate revision 2", st, ev, 201, "")
	st, res := e.admit("agent-a", ev["action"], ev["permit_jws"].(string))
	requireStatus(t, "admit revision 2", st, res, 201, "")

	// Post-commit amendments conflict; still one order and one charge.
	st, body = e.call("POST", path, "issuer-a", amendBody("a2", 2, line("item-a", 1, 20_000_000)))
	requireStatus(t, "amend after commit", st, body, 409, "ALREADY_COMMITTED")
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit - 40_000_000, Committed: 40_000_000, Orders: 1, Obligations: 1, Operations: 1, Outbox: 1})
}

func TestT10AmendmentBeforeAdmission(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	held, release := pauseAt(t, failpoint.AmendAfterLocks)
	amend := async(func() (int, map[string]any) {
		return e.call("POST", "/v1/instructions/"+p.instruction+"/revisions", "issuer-a", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	})
	wait(t, held)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	e.waitLockWaiter()
	release()
	m, a := <-amend, <-admit
	requireStatus(t, "amend", m.status, m.body, 201, "")
	requireStatus(t, "admit", a.status, a.body, 409, "REVISION_STALE")
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
}

func TestT10AdmissionBeforeAmendment(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	held, release := pauseAt(t, failpoint.AdmissionAfterLocks)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	wait(t, held)
	amend := async(func() (int, map[string]any) {
		return e.call("POST", "/v1/instructions/"+p.instruction+"/revisions", "issuer-a", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	})
	e.waitLockWaiter()
	release()
	a, m := <-admit, <-amend
	requireStatus(t, "admit", a.status, a.body, 201, "")
	requireStatus(t, "amend", m.status, m.body, 409, "ALREADY_COMMITTED")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// A signature computed before an amendment cannot reactivate the old issuance.
func TestT10LateSignatureAfterAmendment(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	id := inst["instruction_id"].(string)
	held, release := pauseAt(t, failpoint.EvaluateBeforePersist)
	eval := async(func() (int, map[string]any) { return e.evaluate("agent-a", id, 1) })
	wait(t, held) // reserved and signed; signature not yet persisted
	st, body := e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", amendBody("a1", 1, line("item-a", 2, 20_000_000)))
	requireStatus(t, "amend while signing", st, body, 201, "")
	release()
	r := <-eval
	requireStatus(t, "late signature", r.status, r.body, 409, "REVISION_STALE")
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE permit_jws IS NOT NULL`); n != 0 {
		t.Fatal("late signature was persisted")
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit}) // released exactly once
}

// ---- T13 lost HTTP response ----

func TestT13LostResponse(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	failpoint.Set(failpoint.AdmissionAfterCommit, func(context.Context) error {
		panic(http.ErrAbortHandler) // drop the connection after commit, before any response
	})
	t.Cleanup(failpoint.Clear)
	st, _ := e.admit("agent-a", p.action, p.jws)
	if st != 0 {
		t.Fatalf("client received a response (%d); expected a dropped connection", st)
	}
	failpoint.Clear()
	requireLedger(t, e.ledger(p.mandate), committedLedger) // committed despite the lost response

	st, body := e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "lookup", st, body, 200, "")
	if body["execution"] != "COMMITTED" {
		t.Fatal(body)
	}
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry", st, body, 200, "")
	if body["operation_id"] != p.operation {
		t.Fatal("retry returned a different operation")
	}
	if _, err := e.eng.CleanupExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger) // no release of committed capacity
}

// ---- T14 (in-process) and T16 signing crash ----

func TestT14PrecommitFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	failpoint.Set(failpoint.AdmissionAfterTargetInsert, func(context.Context) error { return errDBDropped })
	t.Cleanup(failpoint.Clear)
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "failed admission", st, body, 503, "SERVICE_UNAVAILABLE")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatalf("permit consumed by a rolled-back admission: %s", s)
	}
	failpoint.Clear()
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry", st, body, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

func TestT16SigningInterruptedThenRetried(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	failpoint.Set(failpoint.EvaluateAfterReserve, func(context.Context) error { return errDBDropped })
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "interrupted evaluate", st, body, 503, "SERVICE_UNAVAILABLE")
	failpoint.Clear()
	var permitID string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT permit_id FROM steelmatter.issuances WHERE status = 'UNSIGNED'`).Scan(&permitID)
	requireLedger(t, e.ledger(m), reservedLedger)

	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "retry completes the same issuance", st, body, 201, "")
	if body["permit"].(map[string]any)["permit_id"] != permitID {
		t.Fatal("retry created a different issuance")
	}
	requireLedger(t, e.ledger(m), reservedLedger) // never double reserved
	if e.count(`SELECT count(*) FROM steelmatter.issuances`) != 1 {
		t.Fatal("more than one issuance")
	}
}

func TestT16UnsignedExpiredIsReleasedByCleanup(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1 * time.Second})
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	failpoint.Set(failpoint.EvaluateAfterReserve, func(context.Context) error { return errDBDropped })
	t.Cleanup(failpoint.Clear)
	st, _ := e.evaluate("agent-a", id, 1)
	if st != 503 {
		t.Fatal(st)
	}
	failpoint.Clear()
	e.waitExpired("")
	if n, err := e.eng.RecoverUnsigned(context.Background()); err != nil || n != 0 {
		t.Fatalf("recovery signed an expired issuance (%d, %v)", n, err)
	}
	if n, err := e.eng.CleanupExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("cleanup released %d (%v); want 1", n, err)
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})
	var reason string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT release_reason FROM steelmatter.issuances`).Scan(&reason)
	if reason != "RECOVERED_UNUSED" {
		t.Fatal(reason)
	}
}

func TestT16RecoveryWorkerSignsSamePayload(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	failpoint.Set(failpoint.EvaluateAfterReserve, func(context.Context) error { return errDBDropped })
	t.Cleanup(failpoint.Clear)
	if st, _ := e.evaluate("agent-a", id, 1); st != 503 {
		t.Fatal(st)
	}
	failpoint.Clear()
	var before []byte
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT permit_canonical FROM steelmatter.issuances`).Scan(&before)
	if n, err := e.eng.RecoverUnsigned(context.Background()); err != nil || n != 1 {
		t.Fatalf("recovered %d (%v)", n, err)
	}
	var jws string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT permit_jws FROM steelmatter.issuances WHERE status = 'ACTIVE'`).Scan(&jws)
	v, verr := contract.Verify(jws, contract.KindPermit, e.eng.Trust)
	if verr != nil || string(v.Payload) != string(before) {
		t.Fatalf("recovered signature is not over the stored payload: %v", verr)
	}
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "retry after recovery", st, body, 200, "")
	requireLedger(t, e.ledger(m), reservedLedger)
}

// ---- T27 unavailable authority ----

func TestT27UnavailableAuthority(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	// Connectivity drops mid-admission: the backend is terminated while the
	// admission holds its locks.
	failpoint.Set(failpoint.AdmissionAfterLocks, func(ctx context.Context) error {
		_, err := e.h.Admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND usename = $2 AND state = 'idle in transaction'`, e.h.DB.Name, e.h.DB.ExecutorRole)
		return err
	})
	t.Cleanup(failpoint.Clear)
	st, body := e.admit("agent-a", p.action, p.jws)
	if st != 503 {
		t.Fatalf("admission without database: %d %v", st, body)
	}
	failpoint.Clear()
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatal("permit changed without a commit")
	}

	// Evaluation against an unreachable database: non-success, no cache allow.
	dead, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	down, err := engine.New(dead, e.eng.Trust, e.eng.Signers, e.eng.Cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = down.Evaluate(context.Background(), principal("agent-a"), p.instruction, 1)
	if f, ok := engine.AsFault(err); !ok || f.Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("evaluate without database: %v", err)
	}

	// Retry with the same permit (same operation identity) succeeds once connectivity returns.
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry", st, body, 201, "")
	if body["operation_id"] != p.operation {
		t.Fatal("retry produced a new operation identity")
	}
}

// ---- T28 cleanup race ----

func TestT28CleanupBeforeAdmission(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1 * time.Second})
	p := e.permitted()
	e.waitExpired("")
	held, release := pauseAt(t, failpoint.CleanupAfterLocks)
	done := make(chan int, 1)
	go func() { n, _ := e.eng.CleanupExpired(context.Background()); done <- n }()
	wait(t, held)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	e.waitLockWaiter()
	release()
	if n := <-done; n != 1 {
		t.Fatalf("cleanup released %d", n)
	}
	a := <-admit
	requireStatus(t, "admission after cleanup", a.status, a.body, 409, "PERMIT_NOT_ACTIVE")
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
}

func TestT28AdmissionHoldsLocksAcrossExpiry(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1 * time.Second})
	p := e.permitted()
	held, release := pauseAt(t, failpoint.AdmissionAfterLocks)
	admit := async(func() (int, map[string]any) { return e.admit("agent-a", p.action, p.jws) })
	wait(t, held)
	e.waitExpired("")
	done := make(chan int, 1)
	go func() { n, _ := e.eng.CleanupExpired(context.Background()); done <- n }()
	e.waitLockWaiter()
	release()
	a := <-admit
	requireStatus(t, "admission rechecks expiry", a.status, a.body, 403, "PERMIT_EXPIRED")
	if n := <-done; n != 1 {
		t.Fatalf("cleanup released %d", n)
	}
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit}) // released once, never committed
}

func TestT28AdmissionCommitsThenCleanupSkips(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1 * time.Second})
	p := e.permitted()
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit", st, body, 201, "")
	e.waitExpired(p.permitID)
	if e.count(`SELECT count(*) FROM steelmatter.issuances WHERE permit_id = $1 AND clock_timestamp() >= expires_at`, p.permitID) != 1 {
		t.Fatal("permit not yet expired; the cleanup check would be vacuous")
	}
	if n, err := e.eng.CleanupExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("cleanup touched a consumed permit (%d, %v)", n, err)
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// ---- T02 failure-injection run ----

func TestT02LoadContentionWithInjectedFailures(t *testing.T) {
	e := newEnv(t)
	m, ids, who := e.loadScenario(100)
	const seed = 20261012
	// The failing requests are fixed by the seed and the instruction index,
	// independent of goroutine scheduling: index i fails before commit,
	// after reserve, or not at all.
	rng := rand.New(rand.NewSource(seed))
	plan := map[string]string{}
	for _, id := range ids {
		switch r := rng.Float64(); {
		case r < 0.2:
			plan[id] = failpoint.EvaluateBeforeReserveCommit
		case r < 0.4:
			plan[id] = failpoint.EvaluateAfterReserve
		}
	}
	inject := func(point string) func(context.Context) error {
		return func(ctx context.Context) error {
			if plan[failpoint.KeyFrom(ctx)] == point {
				return errDBDropped
			}
			return nil
		}
	}
	failpoint.Set(failpoint.EvaluateBeforeReserveCommit, inject(failpoint.EvaluateBeforeReserveCommit)) // rolls back the reservation
	failpoint.Set(failpoint.EvaluateAfterReserve, inject(failpoint.EvaluateAfterReserve))               // reservation kept, signing lost
	t.Cleanup(failpoint.Clear)
	got := tally(e.evaluateConcurrently(who, ids))
	t.Logf("seed %d plan %d before-commit / %d after-reserve failures; outcomes %v", seed, countPlan(plan, failpoint.EvaluateBeforeReserveCommit), countPlan(plan, failpoint.EvaluateAfterReserve), got)
	failed := countPlan(plan, failpoint.EvaluateBeforeReserveCommit) + countPlan(plan, failpoint.EvaluateAfterReserve)
	if got["503:SERVICE_UNAVAILABLE"] > failed {
		t.Fatalf("%d unavailable responses but only %d planned failures", got["503:SERVICE_UNAVAILABLE"], failed)
	}
	if got["PERMIT_ISSUED"] > 10 {
		t.Fatalf("%d permits issued; at most 10 allowed", got["PERMIT_ISSUED"])
	}
	l := e.ledger(m) // asserts conservation and non-negative buckets
	live := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE status IN ('UNSIGNED','ACTIVE')`)
	if l.Reserved != int64(live)*10_000_000 || live > 10 || e.count(`SELECT count(*) FROM steelmatter.issuances WHERE status = 'ACTIVE'`) != got["PERMIT_ISSUED"] {
		t.Fatalf("reserved %d with %d live issuances and %d permits", l.Reserved, live, got["PERMIT_ISSUED"])
	}
}

// errDBDropped simulates losing the database connection at a failpoint.
var errDBDropped = io.ErrUnexpectedEOF

func principal(id string) auth.Principal {
	return auth.Principal{ID: id, Tenant: tenant, Kind: auth.Actor}
}

func countPlan(plan map[string]string, point string) int {
	n := 0
	for _, p := range plan {
		if p == point {
			n++
		}
	}
	return n
}

// T27 over HTTP: connectivity is lost while evaluation holds its locks. The
// request fails (503), nothing is reserved, and a retry succeeds.
func TestT27EvaluateLosesDatabaseOverHTTP(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	failpoint.Set(failpoint.EvaluateBeforeReserveCommit, func(ctx context.Context) error {
		_, err := e.h.Admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND usename = $2 AND state = 'idle in transaction'`, e.h.DB.Name, e.h.DB.ExecutorRole)
		return err
	})
	t.Cleanup(failpoint.Clear)
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate without database", st, body, 503, "SERVICE_UNAVAILABLE")
	if body["retryable"] != true {
		t.Fatal("unavailability must be retryable")
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances`); n != 0 {
		t.Fatalf("%d issuances persisted without a commit", n)
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})
	failpoint.Clear()
	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "retry", st, body, 201, "")
	requireLedger(t, e.ledger(m), reservedLedger)
}

// D-7: a signature computed before a revocation is discarded with the
// precise reason, AUTHORITY_REVOKED.
func TestLateSignatureAfterRevocation(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	held, release := pauseAt(t, failpoint.EvaluateBeforePersist)
	eval := async(func() (int, map[string]any) { return e.evaluate("agent-a", id, 1) })
	wait(t, held)
	st, body := e.call("POST", "/v1/mandates/"+m+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke while signing", st, body, 200, "")
	release()
	r := <-eval
	requireStatus(t, "late signature after revocation", r.status, r.body, 403, "AUTHORITY_REVOKED")
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE permit_jws IS NOT NULL`); n != 0 {
		t.Fatal("late signature was persisted")
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})
}

// serializationFailure simulates PostgreSQL SQLSTATE 40001 at a failpoint.
// (The engine's single lock order makes a real 40P01 deadlock unreachable,
// and READ COMMITTED does not raise 40001, so the retry path is driven by
// injecting the error inside the transaction.)
func serializationFailure() error {
	return &pgconn.PgError{Code: "40001", Message: "injected serialization failure"}
}

// Retries: two serialization failures, then success. The third attempt
// commits with the identities fixed before the first attempt.
func TestRetrySucceedsWithSameIdentities(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	var attempts atomic.Int32
	failpoint.Set(failpoint.AdmissionAfterLocks, func(context.Context) error {
		if attempts.Add(1) <= 2 {
			return serializationFailure()
		}
		return nil
	})
	t.Cleanup(failpoint.Clear)
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit after two retries", st, body, 201, "")
	if attempts.Load() != 3 {
		t.Fatalf("%d attempts; want 3", attempts.Load())
	}
	if body["operation_id"] != p.operation {
		t.Fatal("retry changed the operation identity")
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
	// Exactly one order, obligation and event: no attempt leaked an effect.
	if e.count(`SELECT count(*) FROM target.purchase_orders WHERE order_id = $1`, body["target_ref"].(map[string]any)["order_id"]) != 1 {
		t.Fatal("committed order does not carry the pre-generated order ID")
	}
}

// Exhausted retries: non-success with the existing operation ID, no state
// change, exactly 3 attempts with the specified backoff; a later retry with
// the same permit commits under the same operation.
func TestRetriesExhaustedKeepOperationID(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	var attempts atomic.Int32
	failpoint.Set(failpoint.AdmissionAfterLocks, func(context.Context) error {
		attempts.Add(1)
		return serializationFailure()
	})
	t.Cleanup(failpoint.Clear)
	start := time.Now()
	st, body := e.admit("agent-a", p.action, p.jws)
	elapsed := time.Since(start)
	requireStatus(t, "retries exhausted", st, body, 503, "RETRIES_EXHAUSTED")
	if body["retryable"] != true || body["operation_id"] != p.operation {
		t.Fatalf("exhausted response %v; want retryable with operation_id %s", body, p.operation)
	}
	if attempts.Load() != 3 {
		t.Fatalf("%d attempts; want 3", attempts.Load())
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("finished in %s; the 10 ms + 50 ms backoff was not applied", elapsed)
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	failpoint.Clear()
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "later retry", st, body, 201, "")
	if body["operation_id"] != p.operation {
		t.Fatal("later retry used a new operation identity")
	}
}

// Before the reservation commits there is no operation yet, so an exhausted
// evaluation returns no operation ID (F-18) and persists nothing.
func TestRetriesExhaustedBeforeReservationHasNoOperationID(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	var attempts atomic.Int32
	failpoint.Set(failpoint.EvaluateBeforeReserveCommit, func(context.Context) error {
		attempts.Add(1)
		return serializationFailure()
	})
	t.Cleanup(failpoint.Clear)
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate retries exhausted", st, body, 503, "RETRIES_EXHAUSTED")
	if _, has := body["operation_id"]; has || attempts.Load() != 3 {
		t.Fatalf("response %v after %d attempts; want no operation_id after 3", body, attempts.Load())
	}
	if e.count(`SELECT count(*) FROM steelmatter.issuances`) != 0 {
		t.Fatal("an exhausted evaluation persisted an issuance")
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})
}

// The pinned retry specification (I02 §8).
func TestRetryBackoffSpecification(t *testing.T) {
	if got := engine.DefaultConfig().MaxAttempts; got != 3 {
		t.Fatalf("MaxAttempts %d; I02 §8 specifies 3", got)
	}
	if len(engine.RetryBackoff) != 2 || engine.RetryBackoff[0] != 10*time.Millisecond || engine.RetryBackoff[1] != 50*time.Millisecond {
		t.Fatalf("RetryBackoff %v; I02 §8 specifies [10ms 50ms]", engine.RetryBackoff)
	}
	if engine.TxBudget != 30*time.Second {
		t.Fatalf("TxBudget %s; I02 §8 specifies 30s", engine.TxBudget)
	}
}
