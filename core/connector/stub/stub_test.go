package stub

import (
	"context"
	"sync"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
)

func action(op string) connector.Action {
	return connector.Action{OpRef: op, SupplierRef: "supplier-a", SupplierRevision: 1, AmountMinor: 6000, Currency: "INR",
		PermitDigest: "d1", Lines: []connector.Line{{ItemRef: "chair", Quantity: 1, UnitPriceMinor: 6000}}}
}

func TestCreateIsIdempotentPerOpRef(t *testing.T) {
	s := New()
	ctx := context.Background()
	r1, _ := s.Execute(ctx, action("op-1"))
	r2, _ := s.Execute(ctx, action("op-1"))
	if r1.Outcome != connector.Created || r2.Outcome != connector.Exists || s.Creates() != 1 {
		t.Fatalf("got %s then %s, creates %d", r1.Outcome, r2.Outcome, s.Creates())
	}
	other := action("op-1")
	other.PermitDigest = "d2"
	if r, _ := s.Execute(ctx, other); r.Outcome != connector.Mismatch {
		t.Fatalf("different digest: %s", r.Outcome)
	}
}

func TestDelayedCreateAfterFenceIsRefused(t *testing.T) {
	s := New()
	ctx := context.Background()
	if r, _ := s.Fence(ctx, "op-1", 0); r.Outcome != connector.Fenced {
		t.Fatalf("fence: %s", r.Outcome)
	}
	if r := s.Deliver(action("op-1")); r.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("late create: %s, creates %d", r.Outcome, s.Creates())
	}
	if r, _ := s.Fence(ctx, "op-1", 0); r.Outcome != connector.Fenced {
		t.Fatalf("fence is idempotent: %s", r.Outcome)
	}
}

func TestFenceAfterCreateReportsExists(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.Deliver(action("op-1"))
	if r, _ := s.Fence(ctx, "op-1", 0); r.Outcome != connector.Exists || r.Destination == nil {
		t.Fatalf("fence after create: %+v", r)
	}
}

func TestLostReplyStillCreates(t *testing.T) {
	s := New()
	s.DropNextReplies("op-1", 1)
	r, _ := s.Execute(context.Background(), action("op-1"))
	if r.Outcome != connector.Unknown || s.StateOf("op-1") != Created || s.Creates() != 1 {
		t.Fatalf("lost reply: %s state %q creates %d", r.Outcome, s.StateOf("op-1"), s.Creates())
	}
}

func TestUnreachableHasNoEffect(t *testing.T) {
	s := New()
	s.FailNextCalls("op-1", 1)
	r, _ := s.Execute(context.Background(), action("op-1"))
	if r.Outcome != connector.Unknown || s.StateOf("op-1") != Absent {
		t.Fatalf("unreachable: %s state %q", r.Outcome, s.StateOf("op-1"))
	}
}

func TestSupplierStaleTombstones(t *testing.T) {
	s := New()
	s.SetSupplierRevision("supplier-a", 2)
	r, _ := s.Execute(context.Background(), action("op-1"))
	if r.Outcome != connector.RefusedSupplierStale || !r.ProvesNoOrder() || s.StateOf("op-1") != Refused {
		t.Fatalf("stale supplier: %s state %q", r.Outcome, s.StateOf("op-1"))
	}
	s.SetSupplierRevision("supplier-a", 1)
	if r := s.Deliver(action("op-1")); r.Outcome != connector.RefusedFenced {
		t.Fatalf("retry after refusal: %s", r.Outcome)
	}
}

func TestRestoreGate(t *testing.T) {
	s := New()
	a := action("op-1")
	a.ExpectedMinSeq = 5
	if r, _ := s.Execute(context.Background(), a); r.Outcome != connector.RefusedRestoreSuspected || s.Creates() != 0 {
		t.Fatalf("restore gate: %s", r.Outcome)
	}
}

// Create versus fence, many times: never an order together with FENCED,
// never two orders for one op_ref.
func TestCreateFenceRaceNeverBoth(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		s := New()
		var wg sync.WaitGroup
		var ex, fe connector.Result
		wg.Add(2)
		go func() { defer wg.Done(); ex, _ = s.Execute(ctx, action("op")) }()
		go func() { defer wg.Done(); fe, _ = s.Fence(ctx, "op", 0) }()
		wg.Wait()
		created := ex.Outcome == connector.Created
		fenced := fe.Outcome == connector.Fenced
		if created == fenced || s.Creates() > 1 {
			t.Fatalf("run %d: execute %s, fence %s, creates %d", i, ex.Outcome, fe.Outcome, s.Creates())
		}
	}
}

// D5 v3 case 1 at the destination: a create committed, then a restore erased
// it. A later fence blocks the op_ref but must report the earlier create, so
// the result does not prove no order.
func TestRestoreAfterCreateFenceReportsPriorCreate(t *testing.T) {
	s := New()
	ctx := context.Background()
	before := s.Backup()
	s.LoseNextCreateDone("op-1", 1) // best-effort CREATE_DONE did not land
	s.Deliver(action("op-1"))
	s.Restore(before)
	if s.StateOf("op-1") != Absent {
		t.Fatal("restore did not erase the order")
	}
	r, _ := s.Fence(ctx, "op-1", 0)
	if r.Outcome != connector.Fenced || !is(r.BlockCommitted, true) || !is(r.PriorCreate, true) || r.ProvesNoOrder() || !r.BlockedOverPriorCreate() {
		t.Fatalf("fence after erased create: %+v", r)
	}
}

// D5 v3 case 2 at the destination: a restore removed the in-Odoo tombstone; a
// delayed create carrying an old sequence number is still refused.
func TestDelayedCreateAfterRestoreRemovedBlockIsRefused(t *testing.T) {
	s := New()
	ctx := context.Background()
	before := s.Backup()
	if r, _ := s.Fence(ctx, "op-1", 0); !r.ProvesNoOrder() {
		t.Fatalf("fence: %+v", r)
	}
	s.Restore(before)
	late := action("op-1")
	late.ExpectedMinSeq = 0 // old sequence number
	if r := s.Deliver(late); r.Outcome != connector.RefusedFenced || s.Creates() != 0 {
		t.Fatalf("delayed create after restore: %s, creates %d", r.Outcome, s.Creates())
	}
}

func TestFreshFenceProvesNoOrder(t *testing.T) {
	s := New()
	r, _ := s.Fence(context.Background(), "op-1", 0)
	if !r.ProvesNoOrder() {
		t.Fatalf("fresh fence: %+v", r)
	}
	if r := s.Deliver(action("op-1")); r.ProvesNoOrder() != true || r.Outcome != connector.RefusedFenced {
		t.Fatalf("refusal after committed block: %+v", r)
	}
}

func is(b *bool, want bool) bool { return b != nil && *b == want }

// Kaif's must-fix (case E retry): Odoo restored, CREATE_INTENT present, no
// row. A retry with the same op_ref is refused (REFUSED_PRIOR_ATTEMPT), a
// BLOCK is written and no second order is created.
func TestCaseERetryRefusedPriorAttempt(t *testing.T) {
	s := New()
	ctx := context.Background()
	before := s.Backup()
	if r, _ := s.Execute(ctx, action("op-1")); r.Outcome != connector.Created {
		t.Fatalf("create: %s", r.Outcome)
	}
	s.Restore(before) // Odoo restored: row gone, CREATE_INTENT kept
	r, _ := s.Execute(ctx, action("op-1"))
	if r.Outcome != connector.RefusedPriorAttempt || s.Creates() != 1 {
		t.Fatalf("retry after restore: %s, creates %d", r.Outcome, s.Creates())
	}
	if !is(r.BlockCommitted, true) || !is(r.PriorCreate, true) || r.ProvesNoOrder() {
		t.Fatalf("flags: block %v prior %v proves %v", r.BlockCommitted, r.PriorCreate, r.ProvesNoOrder())
	}
	// The block stands: a later retry is refused too and still no order.
	if r := s.Deliver(action("op-1")); !r.Outcome.Tombstones() || s.Creates() != 1 || r.ProvesNoOrder() {
		t.Fatalf("second retry: %s, creates %d", r.Outcome, s.Creates())
	}
}

// Anchor unreadable during create, fence and lookup: no order, proof flags
// null (unknown, never false), nothing that could release money.
func TestAnchorUnreadableHoldsEverything(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.SetAnchorUnavailable(true)
	ex, _ := s.Execute(ctx, action("op-1"))
	fe, _ := s.Fence(ctx, "op-1", 0)
	lo, _ := s.Lookup(ctx, "op-1")
	for _, r := range []connector.Result{ex, fe, lo} {
		if r.BlockCommitted != nil || r.PriorCreate != nil || r.ProvesNoOrder() || r.BlockedOverPriorCreate() {
			t.Fatalf("%s %s: flags block %v prior %v", r.Call, r.Outcome, r.BlockCommitted, r.PriorCreate)
		}
	}
	if ex.Outcome != connector.Unavailable || fe.Outcome != connector.Unavailable || lo.Outcome != connector.NotFoundNow {
		t.Fatalf("outcomes: execute %s fence %s lookup %s", ex.Outcome, fe.Outcome, lo.Outcome)
	}
	if s.Creates() != 0 || s.StateOf("op-1") != Absent {
		t.Fatalf("anchor down wrote something: creates %d state %q", s.Creates(), s.StateOf("op-1"))
	}
	s.SetAnchorUnavailable(false)
	if r, _ := s.Fence(ctx, "op-1", 0); !r.ProvesNoOrder() {
		t.Fatalf("fence once anchor is back: %+v", r)
	}
}

// CREATE_INTENT written, then the Odoo commit fails: no order, but every later
// result reports prior_create = true, so the operation is HELD and never
// released.
func TestIntentWrittenCommitFailedIsHeld(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.FailNextCommits("op-1", 1)
	if r, _ := s.Execute(ctx, action("op-1")); r.Outcome != connector.Unknown || s.Creates() != 0 || s.StateOf("op-1") != Absent {
		t.Fatalf("failed commit: %s, creates %d", r.Outcome, s.Creates())
	}
	if r, _ := s.Lookup(ctx, "op-1"); r.Outcome != connector.NotFoundNow || !is(r.PriorCreate, true) {
		t.Fatalf("lookup: %s prior %v", r.Outcome, r.PriorCreate)
	}
	r, _ := s.Fence(ctx, "op-1", 0)
	if r.Outcome != connector.Fenced || !is(r.PriorCreate, true) || r.ProvesNoOrder() || !r.BlockedOverPriorCreate() {
		t.Fatalf("fence: %+v", r)
	}
	if r, _ := s.Execute(ctx, action("op-1")); r.ProvesNoOrder() || s.Creates() != 0 {
		t.Fatalf("retry: %s, creates %d", r.Outcome, s.Creates())
	}
}

// BLOCK write fails during fence: the fence rolls back, block_committed =
// false, nothing is released. A later fence that writes BLOCK proves it.
func TestBlockWriteFailsNeverReleases(t *testing.T) {
	s := New()
	ctx := context.Background()
	s.FailNextBlockWrites("op-1", 1)
	r, _ := s.Fence(ctx, "op-1", 0)
	if r.Outcome != connector.Unavailable || !is(r.BlockCommitted, false) || r.ProvesNoOrder() || s.StateOf("op-1") != Absent {
		t.Fatalf("failed block: %s block %v state %q", r.Outcome, r.BlockCommitted, s.StateOf("op-1"))
	}
	if r, _ := s.Fence(ctx, "op-1", 0); r.Outcome != connector.Fenced || !r.ProvesNoOrder() {
		t.Fatalf("retried fence: %+v", r)
	}
}

// CREATE_DONE present after an Odoo restore: the earlier outcome is
// established, so lookup, fence and a retry all report the order (committed),
// not just a hold, and nothing is created twice.
func TestCreateDoneAfterRestoreEstablishesOrder(t *testing.T) {
	s := New()
	ctx := context.Background()
	before := s.Backup()
	s.DropNextReplies("op-1", 1)
	if r, _ := s.Execute(ctx, action("op-1")); r.Outcome != connector.Unknown {
		t.Fatalf("lost reply: %s", r.Outcome)
	}
	s.Restore(before)
	lo, _ := s.Lookup(ctx, "op-1")
	fe, _ := s.Fence(ctx, "op-1", 0)
	ex, _ := s.Execute(ctx, action("op-1"))
	for _, r := range []connector.Result{lo, fe, ex} {
		if !r.CreateDone || r.Destination == nil || r.Destination.PermitDigest != "d1" || r.Destination.AmountMinor != 6000 ||
			r.ProvesNoOrder() || r.BlockedOverPriorCreate() {
			t.Fatalf("%s %s: %+v", r.Call, r.Outcome, r)
		}
	}
	if lo.Outcome != connector.Found || fe.Outcome != connector.Exists || !ex.Outcome.Tombstones() || s.Creates() != 1 {
		t.Fatalf("outcomes: lookup %s fence %s retry %s, creates %d", lo.Outcome, fe.Outcome, ex.Outcome, s.Creates())
	}
}

// A refusal decided before CREATE_INTENT (supplier stale) writes BLOCK only:
// prior_create = false, block committed, so it releases correctly.
func TestRefusalBeforeIntentReleases(t *testing.T) {
	s := New()
	s.SetSupplierRevision("supplier-a", 2)
	r, _ := s.Execute(context.Background(), action("op-1"))
	if r.Outcome != connector.RefusedSupplierStale || !is(r.PriorCreate, false) || !is(r.BlockCommitted, true) || !r.ProvesNoOrder() {
		t.Fatalf("stale supplier: %s block %v prior %v", r.Outcome, r.BlockCommitted, r.PriorCreate)
	}
	if s.Creates() != 0 {
		t.Fatalf("creates %d", s.Creates())
	}
}
