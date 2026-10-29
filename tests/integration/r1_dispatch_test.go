//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// R1 dispatch intent and lookup-first settlement against the stub connector
// (D1 v1, D2 and D4 decided 28 Oct 2026). Every test asserts the persisted
// buckets; ledger() also fails on any I3 violation.

type buckets struct{ Available, Reserved, Committed, Unresolved int64 }

func (e *env) buckets(mandateID string) buckets {
	e.t.Helper()
	l := e.ledger(mandateID)
	return buckets{l.Available, l.Reserved, l.Committed, l.Unresolved}
}

var (
	r1Reserved   = buckets{Available: limit - sixLakh, Reserved: sixLakh}
	r1Unresolved = buckets{Available: limit - sixLakh, Unresolved: sixLakh}
	r1Committed  = buckets{Available: limit - sixLakh, Committed: sixLakh}
	r1Released   = buckets{Available: limit}
)

func requireBuckets(t *testing.T, what string, got, want buckets) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: buckets\n got %+v\nwant %+v", what, got, want)
	}
}

// newR1Env is an engine with a destination connector configured from the
// start, as R1 serve runs: every action is built with the delivery address
// revision the destination requires (Aakhil, 29 Oct 2026). Tests of an
// unconfigured engine set Connector to nil.
func newR1Env(t *testing.T) *env {
	e := newEnv(t)
	e.eng.Connector = stub.New()
	return e
}

func (e *env) dispatch(who string, p permitted) (*engine.DispatchResult, error) {
	e.t.Helper()
	// Dispatch refuses without a configured connector (R1-DEF-12). Tests that
	// settle through their own stub get a ready stand-in here.
	if e.eng.Connector == nil {
		e.eng.Connector = stub.New()
	}
	action, err := json.Marshal(p.action)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.eng.Dispatch(context.Background(), auth.Principal{ID: who, Tenant: tenant, Kind: auth.Actor}, action, p.jws)
}

func (e *env) settle(op string, c connector.Connector) *engine.DispatchResult {
	e.t.Helper()
	r, err := e.eng.Settle(context.Background(), tenant, op, c)
	if err != nil {
		e.t.Fatalf("settle %s: %v", op, err)
	}
	return r
}

func requireExecution(t *testing.T, what string, r *engine.DispatchResult, want string) {
	t.Helper()
	if r == nil || r.Execution != want {
		t.Fatalf("%s: execution %+v, want %s", what, r, want)
	}
}

func mustDispatch(t *testing.T, e *env, p permitted) *engine.DispatchResult {
	t.Helper()
	d, err := e.dispatch("agent-a", p)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	requireExecution(t, "dispatch", d, engine.DispatchPending)
	return d
}

// D2 move 3: dispatch intent consumes the permit and holds the amount as
// unresolved before any network call; an identical repeat changes nothing.
func TestR1DispatchHoldsAmountAsUnresolved(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	requireBuckets(t, "evaluated", e.buckets(p.mandate), r1Reserved)
	d := mustDispatch(t, e, p)
	if d.OpRef != p.operation || !d.Created {
		t.Fatalf("dispatch %+v, want op_ref %s created", d, p.operation)
	}
	requireBuckets(t, "dispatched", e.buckets(p.mandate), r1Unresolved)
	if s, _ := e.issuanceStatus(p.permitID); s != "CONSUMED" {
		t.Fatalf("issuance %s", s)
	}
	again := mustDispatch(t, e, p)
	if again.Created || again.OpRef != d.OpRef {
		t.Fatalf("repeat %+v", again)
	}
	requireBuckets(t, "repeat", e.buckets(p.mandate), r1Unresolved)
	if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'operation.dispatched'`); n != 1 {
		t.Fatalf("dispatched events %d", n)
	}
}

// Success only when the destination confirms a matching order.
func TestR1ConfirmedMatchingOrderCommits(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	r := e.settle(p.operation, s)
	requireExecution(t, "settle", r, engine.DispatchCommitted)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
	if s.Creates() != 1 || r.DestinationOrder == "" {
		t.Fatalf("creates %d, order %q", s.Creates(), r.DestinationOrder)
	}
	// Final: settling again calls nothing and changes nothing.
	requireExecution(t, "again", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "again", e.buckets(p.mandate), r1Committed)
}

// T33 / F1: the order is created but the reply is lost. The amount stays held;
// lookup finds the order; it commits once, with one order.
func TestR1LostReplyHeldThenFound(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.DropNextReplies(p.operation, 1)
	requireExecution(t, "lost reply", e.settle(p.operation, s), engine.DispatchUnresolved)
	if s.StateOf(p.operation) != stub.Created {
		t.Fatal("harness did not create the order")
	}
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "lookup", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "found", e.buckets(p.mandate), r1Committed)
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
}

// F2 and F3: the request never reached the destination. "Not found" alone
// releases nothing; the fence does. A create delayed in transit is then
// refused, so no order appears after the amount was released.
func TestR1NeverReachedIsFencedThenLateCreateRefused(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCalls(p.operation, 1)
	requireExecution(t, "unreachable", e.settle(p.operation, s), engine.DispatchUnresolved)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "lookup then fence", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
	late := s.Deliver(connector.Action{OpRef: p.operation, AmountMinor: sixLakh, Currency: "INR", PermitDigest: "x"})
	if late.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("late create %s, creates %d", late.Outcome, s.Creates())
	}
}

// F4: a delayed create lands while the operation is unresolved; the next
// settle finds it and commits, never fences it.
func TestR1DelayedCreateDuringLookupCommits(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCalls(p.operation, 1)
	requireExecution(t, "unreachable", e.settle(p.operation, s), engine.DispatchUnresolved)
	// The original request arrives late, with the real action.
	if r := s.Deliver(stubActionFor(t, e, p)); r.Outcome != connector.Created {
		t.Fatalf("delayed create %s", r.Outcome)
	}
	requireExecution(t, "lookup", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
}

// F9: the destination stays unreachable; the amount stays held however many
// times the engine retries.
func TestR1UnavailableKeepsHold(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCalls(p.operation, 5)
	for i := 0; i < 5; i++ {
		requireExecution(t, "retry", e.settle(p.operation, s), engine.DispatchUnresolved)
		requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	}
}

// F11: the supplier changed at the destination. The module refuses and
// tombstones op_ref in the same transaction, which counts as FENCED.
func TestR1SupplierStaleRefusalReleases(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.SetSupplierRevision(stubActionFor(t, e, p).SupplierRef, 99)
	r := e.settle(p.operation, s)
	requireExecution(t, "stale supplier", r, engine.DispatchFenced)
	if r.LastOutcome != string(connector.RefusedSupplierStale) {
		t.Fatalf("last outcome %s", r.LastOutcome)
	}
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
}

// F10: an order exists for op_ref but is not a confirmed, matching order. It is
// held for investigation; nothing is committed or released.
func TestR1NonMatchingOrderIsHeld(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.Deliver(stubActionFor(t, e, p))
	s.SetOrderState(p.operation, "draft")
	requireExecution(t, "draft order", e.settle(p.operation, s), engine.DispatchMismatch)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "final", e.settle(p.operation, s), engine.DispatchMismatch)
}

// D4: revocation before the dispatch-intent transaction stops the dispatch
// and releases the reservation; nothing reaches the destination.
func TestR1RevokeBeforeDispatchStopsIt(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	_, err := e.dispatch("agent-a", p)
	if f, ok := engine.AsFault(err); !ok || f.Code != "AUTHORITY_REVOKED" && f.Code != "PERMIT_NOT_ACTIVE" {
		t.Fatalf("dispatch after revoke: %v", err)
	}
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
	if n := e.count(`SELECT count(*) FROM steelmatter.dispatches`); n != 0 {
		t.Fatalf("dispatches %d", n)
	}
}

// D4 (R1-DEF-01): once dispatch intent commits, revocation cannot release the
// amount on its own, and nothing new is ever sent: a PENDING operation is
// treated as sent and settled by lookup and fence only. Unsent, it is fenced
// and released; a late create is refused by the block.
func TestR1RevokeAfterDispatchFencesUnsentOperation(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	p := e.permitted()
	mustDispatch(t, e, p)
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	if inflight, _ := body["in_flight"].([]any); len(inflight) != 1 || inflight[0] != p.operation {
		t.Fatalf("in_flight %v", body["in_flight"])
	}
	if s.Creates() != 0 {
		t.Fatalf("revocation sent a create (%d orders)", s.Creates())
	}
	requireExecution(t, "after revoke", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released by the fence", e.buckets(p.mandate), r1Released)
	if r := s.Deliver(stubActionFor(t, e, p)); r.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("late create after revoke: %s, %d orders", r.Outcome, s.Creates())
	}
}

// D4: an order that already reached the destination before the revoke is
// found by lookup and committed once; revocation never undoes it.
func TestR1RevokeAfterOrderReachedDestinationCommits(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	p := e.permitted()
	mustDispatch(t, e, p)
	s.DropNextReplies(p.operation, 1) // the create applied, its reply lost
	requireExecution(t, "lost reply", e.settle(p.operation, s), engine.DispatchUnresolved)
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	requireExecution(t, "after revoke", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
	if s.Creates() != 1 {
		t.Fatalf("orders %d", s.Creates())
	}
}

// D4 (R1-DEF-01): the settle worker also never sends a PENDING operation of a
// revoked mandate, even when the revoke itself could not reach the destination.
func TestR1WorkerNeverSendsAfterRevoke(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	p := e.permitted()
	mustDispatch(t, e, p)
	e.eng.Connector = nil // the revoke's own settle step cannot run; the worker picks the operation up later
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, body, 200, "")
	if st := e.dispatchStatus(p.operation); st != engine.DispatchPending {
		t.Fatalf("status before the worker: %s", st)
	}
	e.eng.Connector = s
	e.retryNow()
	if _, err := e.eng.SettleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Creates() != 0 {
		t.Fatalf("worker sent a create after revoke (%d orders)", s.Creates())
	}
	if st := e.dispatchStatus(p.operation); st != engine.DispatchFenced {
		t.Fatalf("status %s", st)
	}
	requireBuckets(t, "released by the fence", e.buckets(p.mandate), r1Released)
}

// stubActionFor rebuilds the connector action the engine sends, from the
// stored dispatch, so a test can deliver the "same" request late.
func stubActionFor(t *testing.T, e *env, p permitted) connector.Action {
	t.Helper()
	var digest, company string
	var canonical []byte
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT permit_digest, company_ref, action_canonical FROM steelmatter.dispatches WHERE op_ref = $1`,
		p.operation).Scan(&digest, &company, &canonical); err != nil {
		t.Fatal(err)
	}
	var act contract.Action
	if err := json.Unmarshal(canonical, &act); err != nil {
		t.Fatal(err)
	}
	a := connector.Action{OpRef: p.operation, CompanyRef: company, DeliveryRef: act.DeliveryID, SupplierRef: act.SupplierID, SupplierRevision: act.SupplierRevision, Currency: act.Currency,
		AmountMinor: act.AmountMinor, PermitDigest: digest,
		AmountUntaxedMinor: act.AmountUntaxedMinor, TaxMinor: act.TaxMinor, DeliveryRevision: act.DeliveryRevision}
	for _, l := range act.Lines {
		a.Lines = append(a.Lines, connector.Line{ItemRef: l.ItemID, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor,
			UnitRef: l.UnitID, TaxRefs: l.TaxIDs})
	}
	return a
}

// Concurrent settlers: exactly one records the commit; the amount moves once.
func TestR1ConcurrentSettleMovesAmountOnce(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	done := make(chan *engine.DispatchResult, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, err := e.eng.Settle(context.Background(), tenant, p.operation, s)
			if err != nil {
				t.Errorf("settle: %v", err)
			}
			done <- r
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	requireBuckets(t, "settled", e.buckets(p.mandate), r1Committed)
	if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'operation.committed'`); n != 1 {
		t.Fatalf("committed events %d", n)
	}
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
}

// D5 v3 case 1 (Aakhil, 28 Oct 2026): the order is created, its reply is lost,
// and an old destination backup then erases it. The fence blocks the op_ref,
// but the destination's restore-proof journal shows the earlier create, so the
// earlier outcome is not established: the amount stays held.
func TestR1ErasedEarlierCreateStaysHeld(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	before := s.Backup()
	s.DropNextReplies(p.operation, 1)
	s.LoseNextCreateDone(p.operation, 1) // best-effort CREATE_DONE did not land
	requireExecution(t, "lost reply", e.settle(p.operation, s), engine.DispatchUnresolved)
	if s.StateOf(p.operation) != stub.Created {
		t.Fatal("harness did not create the order")
	}
	s.Restore(before) // old backup erases the order
	r := e.settle(p.operation, s)
	requireExecution(t, "fence over erased create", r, engine.DispatchHeld)
	if r.LastOutcome != string(connector.Fenced) {
		t.Fatalf("last outcome %s", r.LastOutcome)
	}
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "final", e.settle(p.operation, s), engine.DispatchHeld)
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
}

// D5 v3 case 2: a destination restore removes a committed block; a delayed
// request with an old sequence number arrives afterwards. The destination
// refuses it, so the released amount never coexists with an order.
func TestR1DelayedRequestAfterRestoreIsRefused(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCalls(p.operation, 1)
	requireExecution(t, "unreachable", e.settle(p.operation, s), engine.DispatchUnresolved)
	before := s.Backup()
	requireExecution(t, "fence", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
	s.Restore(before) // restore removes the in-Odoo block
	late := stubActionFor(t, e, p)
	late.ExpectedMinSeq = 0
	if r := s.Deliver(late); r.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("delayed request after restore: %s, creates %d", r.Outcome, s.Creates())
	}
	requireBuckets(t, "still released", e.buckets(p.mandate), r1Released)
}

// A refusal counts as FENCED only when the permanent block committed and no
// earlier purchase occurred (Aakhil, 28 Oct 2026). Here an earlier create was
// erased by a restore, so the destination refuses the retry as a prior
// attempt (Kaif's case E fix) and the amount stays held.
func TestR1RefusalAfterEarlierCreateIsHeld(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	before := s.Backup()
	s.LoseNextCreateDone(p.operation, 1)
	s.Deliver(stubActionFor(t, e, p)) // an earlier attempt created the order
	s.Restore(before)                 // and an old backup erased it
	s.SetSupplierRevision(stubActionFor(t, e, p).SupplierRef, 99)
	r := e.settle(p.operation, s)
	requireExecution(t, "refusal over earlier create", r, engine.DispatchHeld)
	if r.LastOutcome != string(connector.RefusedPriorAttempt) {
		t.Fatalf("last outcome %s", r.LastOutcome)
	}
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
}

// Shared interface: company and delivery are part of the match. An order for
// the right op_ref but another company is held, not committed.
func TestR1OrderForAnotherCompanyIsHeld(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	wrong := stubActionFor(t, e, p)
	wrong.CompanyRef = "other-company"
	s.Deliver(wrong)
	requireExecution(t, "other company", e.settle(p.operation, s), engine.DispatchMismatch)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
}

// Anchor unreadable: create and fence report UNAVAILABLE with unknown proof
// flags; the amount stays unresolved and nothing is created.
func TestR1AnchorUnreadableKeepsHold(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.SetAnchorUnavailable(true)
	requireExecution(t, "create", e.settle(p.operation, s), engine.DispatchUnresolved)
	requireExecution(t, "fence", e.settle(p.operation, s), engine.DispatchUnresolved)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	if s.Creates() != 0 {
		t.Fatalf("creates %d", s.Creates())
	}
}

// CREATE_INTENT written, Odoo commit failed: prior_create = true, so the fence
// result is HELD and the amount is never released.
func TestR1IntentWrittenCommitFailedIsHeld(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCommits(p.operation, 1)
	requireExecution(t, "failed commit", e.settle(p.operation, s), engine.DispatchUnresolved)
	requireExecution(t, "fence", e.settle(p.operation, s), engine.DispatchHeld)
	requireExecution(t, "again", e.settle(p.operation, s), engine.DispatchHeld)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
}

// BLOCK write fails during fence: block_committed = false, never released;
// the next fence writes the block and releases.
func TestR1BlockWriteFailedNeverReleases(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.FailNextCalls(p.operation, 1)
	requireExecution(t, "unreachable", e.settle(p.operation, s), engine.DispatchUnresolved)
	s.FailNextBlockWrites(p.operation, 1)
	requireExecution(t, "fence, block write failed", e.settle(p.operation, s), engine.DispatchUnresolved)
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "fence", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
}

// Case 1 with CREATE_DONE: the order was created, the reply lost and an Odoo
// restore erased it, but CREATE_DONE landed, so the earlier outcome is
// established: committed, not just held.
func TestR1CreateDoneAfterRestoreCommits(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	before := s.Backup()
	s.DropNextReplies(p.operation, 1)
	requireExecution(t, "lost reply", e.settle(p.operation, s), engine.DispatchUnresolved)
	s.Restore(before)
	requireExecution(t, "established by CREATE_DONE", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
	if s.Creates() != 1 {
		t.Fatalf("creates %d", s.Creates())
	}
}

// D1 v1.3: a destination refusal for an action outside its configured scope
// is decided before any create attempt and blocks the op_ref, so it releases
// the amount; a later create is refused.
func TestR1OutOfScopeRefusalReleases(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p)
	s := stub.New()
	s.SetOutOfScope(p.operation, "MAPPING_MISSING:item")
	r := e.settle(p.operation, s)
	requireExecution(t, "out of scope", r, engine.DispatchFenced)
	if r.LastOutcome != string(connector.RefusedOutOfScope) {
		t.Fatalf("last outcome %s", r.LastOutcome)
	}
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
	if d := s.Deliver(stubActionFor(t, e, p)); d.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("late create: %s", d.Outcome)
	}
}
