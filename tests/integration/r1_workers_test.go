//go:build integration && failpoints

package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// R1 work that does not depend on the D5 anchor decision: the settle worker,
// operation status, revocation of in-flight operations (D4), per-aggregate
// outbox delivery (D6), the readiness gate (D1) and the D2 budget, crash and
// race tests. All run against the stub connector.

func (e *env) dispatchStatus(op string) string {
	e.t.Helper()
	d, err := e.eng.GetDispatch(context.Background(), tenant, op)
	if err != nil || d == nil {
		e.t.Fatalf("dispatch %s: %+v (%v)", op, d, err)
	}
	return d.Execution
}

// heldVia makes op HELD: created, reply lost, CREATE_DONE lost, then an old
// destination backup erases the order (D5 v3 case 1).
func (e *env) heldVia(op string, s *stub.Stub) {
	e.t.Helper()
	before := s.Backup()
	s.DropNextReplies(op, 1)
	s.LoseNextCreateDone(op, 1)
	requireExecution(e.t, "lost reply", e.settle(op, s), engine.DispatchUnresolved)
	s.Restore(before)
	requireExecution(e.t, "fence over erased create", e.settle(op, s), engine.DispatchHeld)
}

// The settle worker commits what it can, backs off an operation the
// destination cannot answer for, retries it later and fences it; final
// operations are never picked up again.
func TestR1SettleWorkerSettlesAndBacksOff(t *testing.T) {
	e := newEnv(t)
	a := e.permitted()
	b := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	mustDispatch(t, e, a)
	mustDispatch(t, e, b)
	s := stub.New()
	e.eng.Connector = nil
	runWorker(t, "no connector", e.eng.SettleDue, 0, true)
	e.eng.Connector = s
	s.FailNextCalls(b.operation, 2) // execute, then lookup, never reach the destination

	runWorker(t, "first pass", e.eng.SettleDue, 1, false)
	if e.dispatchStatus(a.operation) != engine.DispatchCommitted || e.dispatchStatus(b.operation) != engine.DispatchUnresolved {
		t.Fatalf("after first pass: a %s, b %s", e.dispatchStatus(a.operation), e.dispatchStatus(b.operation))
	}
	if e.backoffAttempts("settle", b.operation) != 1 || e.backoffAttempts("settle", a.operation) != 0 {
		t.Fatal("only the unsettled operation should be backed off")
	}
	runWorker(t, "while backed off", e.eng.SettleDue, 0, false)
	e.retryNow()
	runWorker(t, "lookup unavailable", e.eng.SettleDue, 0, false)
	if e.backoffAttempts("settle", b.operation) != 2 {
		t.Fatalf("attempts %d", e.backoffAttempts("settle", b.operation))
	}
	requireBuckets(t, "b still held", e.buckets(b.mandate), r1Unresolved)
	e.retryNow()
	runWorker(t, "lookup then fence", e.eng.SettleDue, 1, false)
	if e.dispatchStatus(b.operation) != engine.DispatchFenced || e.unresolvedBackoff() != 0 {
		t.Fatalf("b %s, open backoff %d", e.dispatchStatus(b.operation), e.unresolvedBackoff())
	}
	requireBuckets(t, "a committed", e.buckets(a.mandate), r1Committed)
	requireBuckets(t, "b released", e.buckets(b.mandate), r1Released)
	runWorker(t, "nothing left", e.eng.SettleDue, 0, false)
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
}

// HELD and MISMATCH are never settled by the worker; they wait for an
// operator finding and show in the backlog, as does a long-unsettled one.
func TestR1SettleWorkerLeavesHeldAndBacklogReportsIt(t *testing.T) {
	e := newEnv(t)
	a := e.permitted()
	b := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	mustDispatch(t, e, a)
	mustDispatch(t, e, b)
	s := stub.New()
	e.heldVia(a.operation, s)
	e.adminBypass(`UPDATE steelmatter.dispatches SET created_at = clock_timestamp() - interval '10 minutes' WHERE op_ref = $1`, b.operation)
	bl, err := e.eng.WorkerBacklog(context.Background())
	if err != nil || bl.Held != 1 || bl.Settle <= engine.BacklogAlertAge {
		t.Fatalf("backlog %+v (%v)", bl, err)
	}
	e.eng.Connector = s
	runWorker(t, "settle", e.eng.SettleDue, 1, false) // only b
	if e.dispatchStatus(a.operation) != engine.DispatchHeld {
		t.Fatal("worker touched a HELD operation")
	}
	requireBuckets(t, "held", e.buckets(a.mandate), r1Unresolved)
	if bl, _ = e.eng.WorkerBacklog(context.Background()); bl.Settle != 0 || bl.Held != 1 {
		t.Fatalf("backlog after settle %+v", bl)
	}
}

// Operation status reports the dispatch status; only COMMITTED shows an
// order, and HELD is reported as HELD, never as success.
func TestR1OperationStatusReportsDispatchStatus(t *testing.T) {
	e := newEnv(t)
	a := e.permitted()
	mustDispatch(t, e, a)
	status := func() map[string]any {
		st, body := e.call("GET", "/v1/operations/"+a.operation, "observer-a", nil)
		requireStatus(t, "status", st, body, 200, "")
		return body
	}
	if b := status(); b["execution"] != "PENDING" || b["target_ref"] != nil {
		t.Fatalf("pending %v", b)
	}
	s := stub.New()
	s.DropNextReplies(a.operation, 1)
	e.settle(a.operation, s)
	if b := status(); b["execution"] != "UNRESOLVED" || b["target_ref"] != nil {
		t.Fatalf("unresolved %v", b)
	}
	e.settle(a.operation, s)
	b := status()
	ref, _ := b["target_ref"].(map[string]any)
	if b["execution"] != "COMMITTED" || ref == nil || ref["order_id"] == "" {
		t.Fatalf("committed %v", b)
	}

	h := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	mustDispatch(t, e, h)
	e.heldVia(h.operation, stub.New())
	st, body := e.call("GET", "/v1/operations/"+h.operation, "observer-a", nil)
	requireStatus(t, "held status", st, body, 200, "")
	if body["execution"] != "HELD" || body["target_ref"] != nil {
		t.Fatalf("held %v", body)
	}
}

// D4: revoking a mandate lists the operations already dispatched and still
// unsettled, and sends them straight to lookup and fence. Revocation never
// releases their amount; only the destination's proof does.
func TestR1RevokeListsInFlightAndSettlesThem(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	e.eng.Connector = s
	s.DropNextReplies(p.operation, 1) // the order exists, the reply is lost
	runWorker(t, "lost reply", e.eng.SettleDue, 0, false)
	if e.backoffAttempts("settle", p.operation) != 1 {
		t.Fatal("expected the unsettled operation to be backed off")
	}
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	inflight, _ := body["in_flight"].([]any)
	if len(inflight) != 1 || inflight[0] != p.operation {
		t.Fatalf("in_flight %v", body["in_flight"])
	}
	if e.dispatchStatus(p.operation) != engine.DispatchCommitted || e.unresolvedBackoff() != 0 {
		t.Fatalf("after revoke: %s, open backoff %d", e.dispatchStatus(p.operation), e.unresolvedBackoff())
	}
	requireBuckets(t, "committed despite revoke", e.buckets(p.mandate), r1Committed)
}

func TestR1RevokeWithUnreachableDestinationKeepsHold(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	e.eng.Connector = s
	s.FailNextCalls(p.operation, 2) // execute (worker) and lookup (revoke)
	runWorker(t, "unreachable", e.eng.SettleDue, 0, false)
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	if l, _ := body["in_flight"].([]any); len(l) != 1 {
		t.Fatalf("in_flight %v", body["in_flight"])
	}
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
	runWorker(t, "worker retries at once", e.eng.SettleDue, 1, false) // backoff was cleared by the revoke
	if e.dispatchStatus(p.operation) != engine.DispatchFenced {
		t.Fatalf("status %s", e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "released by the fence", e.buckets(p.mandate), r1Released)

	// A mandate with nothing in flight reports an empty list.
	q := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	st, body = e.call("POST", "/v1/mandates/"+q.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke empty", st, body, 200, "")
	if l, ok := body["in_flight"].([]any); !ok || len(l) != 0 {
		t.Fatalf("in_flight %v", body["in_flight"])
	}
}

// D1 readiness: NOT_READY blocks dispatch before anything is consumed or
// moved; the same permit works once the destination is ready.
func TestR1ReadinessBlocksDispatchWithoutConsumingPermit(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	s := stub.New()
	e.eng.Connector = s
	for _, c := range []struct {
		setup func()
		code  string
	}{
		{func() { s.SetSupplierRevision("supplier-a", 2) }, "SUPPLIER_REVISION_STALE"},
		{func() { s.SetSupplierRevision("supplier-a", 1); s.SetNotReady(connector.ReasonMappingMissing) }, "ADAPTER_UNSUPPORTED"},
		{func() { s.SetNotReady(""); s.FailNextCalls(p.operation, 1) }, "SERVICE_UNAVAILABLE"},
	} {
		c.setup()
		_, err := e.dispatch("agent-a", p)
		if f, ok := engine.AsFault(err); !ok || f.Code != c.code {
			t.Fatalf("want %s, got %v", c.code, err)
		}
		if st, _ := e.issuanceStatus(p.permitID); st != "ACTIVE" {
			t.Fatalf("permit %s after NOT_READY", st)
		}
		requireBuckets(t, "nothing moved", e.buckets(p.mandate), r1Reserved)
	}
	mustDispatch(t, e, p)
	requireBuckets(t, "dispatched", e.buckets(p.mandate), r1Unresolved)
}

// ---- D6: per-aggregate outbox ----

type recordingSink struct {
	mu        sync.Mutex
	failAggr  string
	delivered []engine.Event
}

func (s *recordingSink) Deliver(_ context.Context, ev engine.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.AggregateID == s.failAggr {
		return errors.New("consumer refused")
	}
	s.delivered = append(s.delivered, ev)
	return nil
}

func (s *recordingSink) seqs(aggregate string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, ev := range s.delivered {
		if ev.AggregateID == aggregate {
			out = append(out, ev.AggregateSeq)
		}
	}
	return out
}

// A permanently failing event holds back only the later events of its own
// operation; every other aggregate is delivered, and once the consumer
// recovers the held events arrive in aggregate_seq order.
func TestD6FailingEventHoldsOnlyItsAggregate(t *testing.T) {
	e := newEnv(t)
	a := e.permitted()
	b := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-002")
	mustDispatch(t, e, a)
	mustDispatch(t, e, b)
	s := stub.New()
	s.DropNextReplies(a.operation, 1)
	e.settle(a.operation, s) // a: dispatched, unresolved
	e.settle(a.operation, s) // a: committed
	e.settle(b.operation, s) // b: dispatched, committed
	if got := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE aggregate_id = $1 AND aggregate_seq IN (1, 2, 3)`, a.operation); got != 3 {
		t.Fatalf("aggregate_seq for a: %d of 3", got)
	}
	sink := &recordingSink{failAggr: a.operation}
	n, err := e.eng.DispatchOutbox(context.Background(), sink)
	if err == nil {
		t.Fatal("expected the failing event to be reported")
	}
	if got := sink.seqs(b.operation); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("b delivered %v", got)
	}
	if len(sink.seqs(a.operation)) != 0 || n == 0 {
		t.Fatalf("a delivered %v, total %d", sink.seqs(a.operation), n)
	}
	if e.count(`SELECT count(*) FROM steelmatter.outbox WHERE aggregate_id = $1 AND delivered_at IS NULL`, a.operation) != 3 {
		t.Fatal("a's later events must wait behind its failing head")
	}
	if e.count(`SELECT count(*) FROM steelmatter.worker_backoff WHERE worker = 'outbox' AND resolved_at IS NULL`) != 1 {
		t.Fatal("only the head event should be backed off")
	}
	e.adminBypass(`UPDATE steelmatter.outbox SET created_at = clock_timestamp() - interval '10 minutes' WHERE aggregate_id = $1`, a.operation)
	if bl, _ := e.eng.WorkerBacklog(context.Background()); bl.Outbox <= engine.BacklogAlertAge {
		t.Fatalf("backlog %+v", bl)
	}
	sink.failAggr = ""
	e.retryNow()
	if _, err := e.eng.DispatchOutbox(context.Background(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.seqs(a.operation); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("a delivered %v", got)
	}
	if e.count(`SELECT count(*) FROM steelmatter.outbox WHERE delivered_at IS NULL`) != 0 || e.unresolvedBackoff() != 0 {
		t.Fatal("outbox not drained")
	}
}

// ---- D2: budget property, crash points and the expiry race ----

// requireBudgetMatchesRecords checks the D2 invariant for one mandate: the
// buckets sum to the limit (ledger() checks I3) and the unresolved and
// committed buckets equal the amounts of the dispatch records in those
// states. Reserved equals the active permits.
func (e *env) requireBudgetMatchesRecords(what, mandate string) {
	e.t.Helper()
	l := e.ledger(mandate)
	held := int64(e.count(`SELECT COALESCE(sum(amount_minor), 0)::int FROM steelmatter.dispatches WHERE mandate_id = $1
		AND status IN ('PENDING', 'UNRESOLVED', 'MISMATCH', 'HELD')`, mandate))
	committed := int64(e.count(`SELECT COALESCE(sum(amount_minor), 0)::int FROM steelmatter.dispatches WHERE mandate_id = $1 AND status = 'COMMITTED'`, mandate))
	reserved := int64(e.count(`SELECT COALESCE(sum(amount_minor), 0)::int FROM steelmatter.issuances WHERE mandate_id = $1 AND status IN ('UNSIGNED', 'ACTIVE')`, mandate))
	if l.Unresolved != held || l.Committed != committed || l.Reserved != reserved {
		e.t.Fatalf("%s: buckets %+v; records: unresolved %d committed %d reserved %d", what, l, held, committed, reserved)
	}
}

// Random sequences of dispatches and destination behaviours on one mandate:
// after every step the buckets are nonnegative, sum to the limit and match
// the operation records; nothing is ever released while an order may exist.
func TestD2BudgetPropertyUnderRandomOutcomes(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	const seed, n = 20261028, 12
	rng := rand.New(rand.NewSource(seed))
	s := stub.New()
	var ops []permitted
	for i := 0; i < n; i++ {
		inst := e.instruction(fmt.Sprintf("po-prop-%03d", i), m, line("item-a", 1, int64(1_000_000+rng.Intn(5_000_000))))
		id := inst["instruction_id"].(string)
		st, body := e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
		permit := body["permit"].(map[string]any)
		p := permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
			permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
		e.requireBudgetMatchesRecords("evaluated", m)
		if rng.Intn(5) == 0 {
			continue // never dispatched: stays reserved
		}
		mustDispatch(t, e, p)
		ops = append(ops, p)
		e.requireBudgetMatchesRecords("dispatched", m)
		switch rng.Intn(6) {
		case 0:
			s.DropNextReplies(p.operation, 1)
		case 1:
			s.FailNextCalls(p.operation, 1+rng.Intn(3))
		case 2:
			s.SetSupplierRevision("supplier-a", 2) // this one is refused; reset below
		case 3:
			before := s.Backup()
			s.DropNextReplies(p.operation, 1)
			s.LoseNextCreateDone(p.operation, 1)
			e.settle(p.operation, s)
			s.Restore(before) // erased earlier create: must end HELD, never released
		case 4:
			s.FailNextBlockWrites(p.operation, 1)
			s.FailNextCalls(p.operation, 1)
		}
		e.settle(p.operation, s)
		s.SetSupplierRevision("supplier-a", 1)
		e.requireBudgetMatchesRecords("settled once", m)
	}
	for round := 0; round < 6; round++ {
		for _, p := range ops {
			e.settle(p.operation, s)
			e.requireBudgetMatchesRecords(fmt.Sprintf("round %d", round), m)
		}
	}
	for _, p := range ops {
		st := e.dispatchStatus(p.operation)
		order := s.StateOf(p.operation) == stub.Created
		if st == engine.DispatchFenced && order {
			t.Fatalf("%s released while an order exists", p.operation)
		}
		if st == engine.DispatchCommitted && !order {
			t.Fatalf("%s committed without an order", p.operation)
		}
		if st == engine.DispatchPending || st == engine.DispatchUnresolved {
			t.Fatalf("%s still %s after the retry rounds", p.operation, st)
		}
	}
}

// Crash points around dispatch and settle: the amount is always in exactly
// one bucket and never available while an order may exist.
func TestD2CrashBeforeDispatchCommitLeavesReservation(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	failpoint.Set(failpoint.DispatchBeforeCommit, func(context.Context) error { return failpoint.ErrInjected })
	t.Cleanup(failpoint.Clear)
	if _, err := e.dispatch("agent-a", p); err == nil {
		t.Fatal("dispatch should fail at the crash point")
	}
	requireBuckets(t, "rolled back", e.buckets(p.mandate), r1Reserved)
	if st, _ := e.issuanceStatus(p.permitID); st != "ACTIVE" || e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 {
		t.Fatalf("permit %s, dispatches %d", st, e.count(`SELECT count(*) FROM steelmatter.dispatches`))
	}
	failpoint.Clear()
	mustDispatch(t, e, p)
	e.requireBudgetMatchesRecords("retried", p.mandate)
}

func TestD2CrashAfterDispatchBeforeSendHoldsAmount(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	mustDispatch(t, e, p) // committed; the process "dies" before any network call
	requireBuckets(t, "held before send", e.buckets(p.mandate), r1Unresolved)
	// A restarted worker sends it with the same op_ref.
	e.eng.Connector = stub.New()
	runWorker(t, "after restart", e.eng.SettleDue, 1, false)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
}

func TestD2CrashAfterReplyBeforeRecordCommitsOnce(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	failpoint.Set(failpoint.SettleBeforeRecord, func(context.Context) error { return failpoint.ErrInjected })
	t.Cleanup(failpoint.Clear)
	if _, err := e.eng.Settle(context.Background(), tenant, p.operation, s); err == nil {
		t.Fatal("settle should fail at the crash point")
	}
	if s.StateOf(p.operation) != stub.Created || e.dispatchStatus(p.operation) != engine.DispatchPending {
		t.Fatalf("destination %q, engine %s", s.StateOf(p.operation), e.dispatchStatus(p.operation))
	}
	requireBuckets(t, "held after crash", e.buckets(p.mandate), r1Unresolved)
	failpoint.Clear()
	requireExecution(t, "retry", e.settle(p.operation, s), engine.DispatchCommitted)
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
	requireBuckets(t, "committed once", e.buckets(p.mandate), r1Committed)
}

// Dispatch holds the admission locks while the permit expires; cleanup
// waits. Dispatch then rechecks expiry and refuses, and cleanup releases the
// reservation once. Released or dispatched, never both.
func TestD2DispatchHoldsLocksAcrossExpiry(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1e9})
	p := e.permitted()
	held, release := pauseAt(t, failpoint.AdmissionAfterLocks)
	errc := make(chan error, 1)
	go func() { _, err := e.dispatch("agent-a", p); errc <- err }()
	wait(t, held)
	e.waitExpired("")
	done := make(chan int, 1)
	go func() { n, _ := e.eng.CleanupExpired(context.Background()); done <- n }()
	e.waitLockWaiter()
	release()
	if f, ok := engine.AsFault(<-errc); !ok || (f.Code != "PERMIT_EXPIRED" && f.Code != "PERMIT_NOT_ACTIVE") {
		t.Fatal("dispatch should refuse the expired permit")
	}
	if n := <-done; n != 1 {
		t.Fatalf("cleanup released %d", n)
	}
	requireBuckets(t, "released once, never dispatched", e.buckets(p.mandate), r1Released)
	if e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 {
		t.Fatal("a dispatch was recorded")
	}
}

// Dispatch commits first; cleanup after expiry finds the permit consumed and
// leaves the unresolved amount alone.
func TestD2DispatchThenCleanupSkips(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 1e9})
	p := e.permitted()
	mustDispatch(t, e, p)
	e.waitExpired(p.permitID)
	if n, err := e.eng.CleanupExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("cleanup touched a consumed permit (%d, %v)", n, err)
	}
	requireBuckets(t, "still unresolved", e.buckets(p.mandate), r1Unresolved)
}
