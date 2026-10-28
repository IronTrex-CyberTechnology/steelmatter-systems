// Package stub is an in-memory connector.Connector that follows the agreed
// Odoo module semantics (D1 v1, D3, D5 v3): one record per op_ref, create and
// fence serialised on it, permanent tombstones, a commit-ordered sequence and
// the expected_min_seq restore gate. It also models the destination's
// restore-proof record (the "journal outside Odoo's rollback"): a create
// intent is written there before a create commits, and a block before a
// fence or refusal is reported. Restore rolls back the in-Odoo state but not
// that record.
//
// It injects failures (lost replies, unreachable destination, delayed creates,
// destination restores) and records what really happened, so a test can show
// that a simulated failure actually occurred.
package stub

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
)

// State is the destination record state for one op_ref.
type State string

const (
	Absent  State = ""
	Created State = "created"
	Fenced  State = "fenced"
	Refused State = "refused"
)

type record struct {
	state  State
	digest string
	order  *connector.Order
	seq    int64
}

// durable is the per-op_ref restore-proof record; it survives Restore.
type durable struct {
	createIntent bool             // CREATE_INTENT: a create was attempted (written before it commits)
	blocked      bool             // BLOCK: a permanent block committed
	createDone   *connector.Order // CREATE_DONE: best-effort, after the create committed; carries the order facts
}

// Stub is safe for concurrent use. The zero value is not usable; call New.
type Stub struct {
	mu          sync.Mutex
	ops         map[string]*record
	seq         int64
	nextOrder   int
	supplierRev map[string]int64
	dropReply   map[string]int // op_ref -> execute replies to drop after the create applies
	unreachable map[string]int // op_ref -> calls that fail before reaching the destination
	creates     int
	durable     map[string]*durable

	anchorDown     bool           // restore-proof record unreadable and unwritable
	failCommit     map[string]int // op_ref -> creates whose destination commit fails after CREATE_INTENT
	failBlock      map[string]int // op_ref -> fences whose BLOCK write fails
	loseCreateDone map[string]int // op_ref -> creates whose best-effort CREATE_DONE write is lost

	notReady string              // readiness reason to report, if set
	foreign  []connector.ListRow // orders created outside Steelmatter
}

// Snapshot is a destination backup: the in-Odoo state only.
type Snapshot struct {
	ops       map[string]record
	seq       int64
	nextOrder int
}

// New returns an empty stub destination.
func New() *Stub {
	return &Stub{ops: map[string]*record{}, supplierRev: map[string]int64{}, dropReply: map[string]int{}, unreachable: map[string]int{},
		durable: map[string]*durable{}, failCommit: map[string]int{}, failBlock: map[string]int{}, loseCreateDone: map[string]int{}}
}

// SetAnchorUnavailable makes the destination's restore-proof record
// unreadable and unwritable (or available again).
func (s *Stub) SetAnchorUnavailable(down bool) {
	s.mu.Lock()
	s.anchorDown = down
	s.mu.Unlock()
}

// FailNextCommits makes the next n creates for opRef write CREATE_INTENT and
// then fail their destination commit (no order; reply UNKNOWN).
func (s *Stub) FailNextCommits(opRef string, n int) {
	s.mu.Lock()
	s.failCommit[opRef] = n
	s.mu.Unlock()
}

// FailNextBlockWrites makes the next n fences for opRef fail to write BLOCK;
// the fence then rolls back and reports UNAVAILABLE.
func (s *Stub) FailNextBlockWrites(opRef string, n int) {
	s.mu.Lock()
	s.failBlock[opRef] = n
	s.mu.Unlock()
}

// LoseNextCreateDone makes the best-effort CREATE_DONE write of the next n
// creates for opRef fail (the create itself still commits).
func (s *Stub) LoseNextCreateDone(opRef string, n int) {
	s.mu.Lock()
	s.loseCreateDone[opRef] = n
	s.mu.Unlock()
}

func take(m map[string]int, k string) bool {
	if m[k] > 0 {
		m[k]--
		return true
	}
	return false
}

// Backup takes a destination backup.
func (s *Stub) Backup() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := Snapshot{ops: map[string]record{}, seq: s.seq, nextOrder: s.nextOrder}
	for k, r := range s.ops {
		c := *r
		if r.order != nil {
			o := *r.order
			c.order = &o
		}
		b.ops[k] = c
	}
	return b
}

// Restore rolls the in-Odoo state back to b, as an Odoo database restore
// would. The restore-proof record is outside that rollback and is kept.
func (s *Stub) Restore(b Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = map[string]*record{}
	for k, r := range b.ops {
		c := r
		s.ops[k] = &c
	}
	s.seq, s.nextOrder = b.seq, b.nextOrder
}

func (s *Stub) dur(opRef string) *durable {
	d := s.durable[opRef]
	if d == nil {
		d = &durable{}
		s.durable[opRef] = d
	}
	return d
}

// DropNextReplies makes the next n executes for opRef apply at the
// destination but return UNKNOWN, as if the reply was lost.
func (s *Stub) DropNextReplies(opRef string, n int) {
	s.mu.Lock()
	s.dropReply[opRef] = n
	s.mu.Unlock()
}

// FailNextCalls makes the next n calls for opRef fail before reaching the
// destination (no effect).
func (s *Stub) FailNextCalls(opRef string, n int) {
	s.mu.Lock()
	s.unreachable[opRef] = n
	s.mu.Unlock()
}

// SetSupplierRevision sets the destination's protected supplier revision.
func (s *Stub) SetSupplierRevision(supplierRef string, rev int64) {
	s.mu.Lock()
	s.supplierRev[supplierRef] = rev
	s.mu.Unlock()
}

// SetOrderState changes an existing order's state, as an Odoo user might.
func (s *Stub) SetOrderState(opRef, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.ops[opRef]; r != nil && r.order != nil {
		r.order.State = state
	}
}

// Deliver applies a create that was delayed in transit, with the same
// semantics as Execute but without fault injection. It returns what the
// destination did.
func (s *Stub) Deliver(a connector.Action) connector.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(a)
}

// StateOf reports the destination record state for opRef.
func (s *Stub) StateOf(opRef string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.ops[opRef]; r != nil {
		return r.state
	}
	return Absent
}

// Creates is the number of orders the destination has ever created.
func (s *Stub) Creates() int { s.mu.Lock(); defer s.mu.Unlock(); return s.creates }

// Head is the destination's current sequence.
func (s *Stub) Head() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.seq }

func (s *Stub) result(opRef, call string, o connector.Outcome) connector.Result {
	r := connector.Result{OpRef: opRef, Call: call, Outcome: o, Seq: s.seq, Incarnation: "stub-1",
		OdooTxTime: time.Now().UTC(), ConnectorReceivedAt: time.Now().UTC(), Source: "stub/executor"}
	if !s.anchorDown {
		d := s.durable[opRef]
		if d == nil {
			d = &durable{}
		}
		r.BlockCommitted, r.PriorCreate = connector.Flag(d.blocked), connector.Flag(d.createIntent)
		if rec := s.ops[opRef]; d.createDone != nil && (rec == nil || rec.order == nil) {
			// The order was erased (restore); the CREATE_DONE record still
			// establishes it.
			o := *d.createDone
			o.Lines = append([]connector.Line(nil), d.createDone.Lines...)
			r.CreateDone, r.Destination = true, &o
			r.Reason = "ORDER_ERASED_CREATE_DONE"
		}
	}
	if rec := s.ops[opRef]; rec != nil {
		r.Seq = rec.seq
		if rec.order != nil {
			o := *rec.order
			o.Lines = append([]connector.Line(nil), rec.order.Lines...)
			r.Destination = &o
		}
	}
	return r
}

func (s *Stub) failing(opRef string) bool {
	if s.unreachable[opRef] > 0 {
		s.unreachable[opRef]--
		return true
	}
	return false
}

// create runs under s.mu: the per-op_ref lock of the real module.
func (s *Stub) create(a connector.Action) connector.Result {
	if a.ExpectedMinSeq > s.seq {
		return s.result(a.OpRef, "execute", connector.RefusedRestoreSuspected)
	}
	// The block check needs the restore-proof record; if it cannot be read
	// the create is refused (fail closed) and nothing is written.
	if s.anchorDown {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unavailable, Reason: "ANCHOR_UNAVAILABLE", Seq: s.seq}
	}
	// The restore-proof block is checked first, so a delayed create cannot
	// slip through after a restore removed the in-Odoo tombstone, whatever
	// sequence number it carries (D5 v3 case 2).
	if d := s.durable[a.OpRef]; d != nil && d.blocked {
		if rec := s.ops[a.OpRef]; rec == nil {
			s.ops[a.OpRef] = &record{state: Fenced, seq: s.seq}
		}
		return s.result(a.OpRef, "execute", connector.RefusedFenced)
	}
	rec := s.ops[a.OpRef]
	if rec != nil {
		switch rec.state {
		case Fenced, Refused:
			return s.result(a.OpRef, "execute", connector.RefusedFenced)
		case Created:
			if rec.digest == a.PermitDigest {
				return s.result(a.OpRef, "execute", connector.Exists)
			}
			return s.result(a.OpRef, "execute", connector.Mismatch)
		}
	}
	// A create intent with no destination row means an earlier attempt was
	// erased by a restore (or its commit failed). Never create again for this
	// op_ref: block it and refuse (Kaif's case E fix).
	if d := s.durable[a.OpRef]; d != nil && d.createIntent {
		s.dur(a.OpRef).blocked = true
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		return s.result(a.OpRef, "execute", connector.RefusedPriorAttempt)
	}
	// Refusals decided before CREATE_INTENT write BLOCK only, so they carry
	// prior_create = false and can release.
	if want, ok := s.supplierRev[a.SupplierRef]; ok && want != a.SupplierRevision {
		s.dur(a.OpRef).blocked = true // block recorded before the refusal is reported
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		return s.result(a.OpRef, "execute", connector.RefusedSupplierStale)
	}
	s.dur(a.OpRef).createIntent = true // written before the create commits
	if take(s.failCommit, a.OpRef) {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "COMMIT_FAILED", Seq: s.seq}
	}
	s.seq++
	s.nextOrder++
	s.creates++
	s.ops[a.OpRef] = &record{state: Created, digest: a.PermitDigest, seq: s.seq, order: &connector.Order{
		ID: fmt.Sprintf("PO%05d", s.nextOrder), State: connector.StateConfirmed, CompanyRef: a.CompanyRef, DeliveryRef: a.DeliveryRef,
		SupplierRef: a.SupplierRef, Lines: append([]connector.Line(nil), a.Lines...), AmountMinor: a.AmountMinor, Currency: a.Currency,
		PermitDigest: a.PermitDigest,
	}}
	if !take(s.loseCreateDone, a.OpRef) {
		o := *s.ops[a.OpRef].order
		o.Lines = append([]connector.Line(nil), a.Lines...)
		s.dur(a.OpRef).createDone = &o
	}
	return s.result(a.OpRef, "execute", connector.Created)
}

// Execute implements connector.Connector.
func (s *Stub) Execute(_ context.Context, a connector.Action) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(a.OpRef) {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	r := s.create(a)
	if s.dropReply[a.OpRef] > 0 {
		s.dropReply[a.OpRef]--
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "REPLY_LOST"}, nil
	}
	return r, nil
}

// Lookup implements connector.Connector.
func (s *Stub) Lookup(_ context.Context, opRef string) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(opRef) {
		return connector.Result{OpRef: opRef, Call: "lookup", Outcome: connector.Unavailable, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	rec := s.ops[opRef]
	switch {
	case rec == nil && !s.anchorDown && s.durable[opRef] != nil && s.durable[opRef].createDone != nil:
		return s.result(opRef, "lookup", connector.Found), nil // order erased; CREATE_DONE establishes it
	case rec == nil:
		return s.result(opRef, "lookup", connector.NotFoundNow), nil
	case rec.state == Fenced || rec.state == Refused:
		return s.result(opRef, "lookup", connector.Fenced), nil
	case rec.order.State != connector.StateConfirmed:
		return s.result(opRef, "lookup", connector.Mismatch), nil
	default:
		return s.result(opRef, "lookup", connector.Found), nil
	}
}

// Fence implements connector.Connector.
func (s *Stub) Fence(_ context.Context, opRef string, expectedMinSeq int64) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(opRef) {
		return connector.Result{OpRef: opRef, Call: "fence", Outcome: connector.Unknown, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	if expectedMinSeq > s.seq {
		return s.result(opRef, "fence", connector.RefusedRestoreSuspected), nil
	}
	if s.anchorDown {
		return connector.Result{OpRef: opRef, Call: "fence", Outcome: connector.Unavailable, Reason: "ANCHOR_UNAVAILABLE", Seq: s.seq}, nil
	}
	rec := s.ops[opRef]
	switch {
	case rec == nil && take(s.failBlock, opRef):
		// BLOCK write failed: the fence transaction rolls back, nothing is
		// blocked, and the flag says so.
		r := s.result(opRef, "fence", connector.Unavailable)
		r.Reason, r.BlockCommitted = "BLOCK_WRITE_FAILED", connector.Flag(false)
		return r, nil
	case rec == nil && s.durable[opRef] != nil && s.durable[opRef].createDone != nil:
		// Order erased by a restore but CREATE_DONE establishes it: block
		// any re-create and report the order (case E).
		s.dur(opRef).blocked = true
		s.seq++
		s.ops[opRef] = &record{state: Fenced, seq: s.seq}
		return s.result(opRef, "fence", connector.Exists), nil
	case rec == nil:
		s.dur(opRef).blocked = true // block recorded before FENCED is reported
		s.seq++
		s.ops[opRef] = &record{state: Fenced, seq: s.seq}
		return s.result(opRef, "fence", connector.Fenced), nil
	case rec.state == Created:
		return s.result(opRef, "fence", connector.Exists), nil
	default:
		s.dur(opRef).blocked = true
		return s.result(opRef, "fence", connector.Fenced), nil
	}
}

// SetNotReady makes Readiness report NOT_READY with reason (empty clears it).
func (s *Stub) SetNotReady(reason string) {
	s.mu.Lock()
	s.notReady = reason
	s.mu.Unlock()
}

// AddForeignOrder records an order created directly in the destination,
// outside Steelmatter (no op_ref).
func (s *Stub) AddForeignOrder(amountMinor int64, currency string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.nextOrder++
	id := fmt.Sprintf("PO%05d", s.nextOrder)
	s.foreign = append(s.foreign, connector.ListRow{Seq: s.seq, State: "foreign", OrderID: id, AmountMinor: amountMinor, Currency: currency, Foreign: true})
	return id
}

// Readiness implements connector.Connector. Advisory only.
func (s *Stub) Readiness(_ context.Context, a connector.Action) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := connector.Result{OpRef: a.OpRef, Call: "readiness", Outcome: connector.Ready, Seq: s.seq, Source: "stub/executor"}
	switch {
	case s.failing(a.OpRef):
		r.Outcome, r.Reason = connector.NotReady, connector.ReasonDestinationUnavailable
	case s.notReady != "":
		r.Outcome, r.Reason = connector.NotReady, s.notReady
	default:
		if want, ok := s.supplierRev[a.SupplierRef]; ok && want != a.SupplierRevision {
			r.Outcome, r.Reason = connector.NotReady, connector.ReasonSupplierChanged
		}
	}
	return r, nil
}

// Observe implements connector.Connector (observer identity; read-only).
func (s *Stub) Observe(_ context.Context, a connector.Action) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := connector.Result{OpRef: a.OpRef, Call: "observe", Seq: s.seq, Source: "stub/observer"}
	rec := s.ops[a.OpRef]
	switch {
	case rec == nil || rec.order == nil:
		r.Outcome = connector.NotFound
	case rec.order.State != connector.StateConfirmed:
		r.Outcome = connector.InsufficientEvidence
	default:
		o := rec.order
		r.Outcome = connector.Match
		if o.CompanyRef != a.CompanyRef || o.DeliveryRef != a.DeliveryRef || o.SupplierRef != a.SupplierRef || o.AmountMinor != a.AmountMinor ||
			o.Currency != a.Currency || o.PermitDigest != a.PermitDigest || len(o.Lines) != len(a.Lines) {
			r.Outcome = connector.Mismatch
		} else {
			for i := range a.Lines {
				if o.Lines[i] != a.Lines[i] {
					r.Outcome = connector.Mismatch
				}
			}
		}
		c := *o
		c.Lines = append([]connector.Line(nil), o.Lines...)
		r.Destination = &c
	}
	return r, nil
}

// List implements connector.Connector (sm_list_since; observer identity).
func (s *Stub) List(_ context.Context, checkpointSeq int64, limit int) (connector.ListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []connector.ListRow
	for op, rec := range s.ops {
		if rec.seq <= checkpointSeq {
			continue
		}
		row := connector.ListRow{Seq: rec.seq, OpRef: op, State: string(rec.state), PermitDigest: rec.digest, Tombstoned: rec.state == Fenced || rec.state == Refused}
		if rec.order != nil {
			row.OrderID, row.AmountMinor, row.Currency = rec.order.ID, rec.order.AmountMinor, rec.order.Currency
		}
		rows = append(rows, row)
	}
	for _, f := range s.foreign {
		if f.Seq > checkpointSeq {
			rows = append(rows, f)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	next := checkpointSeq
	if len(rows) > 0 {
		next = rows[len(rows)-1].Seq
	}
	return connector.ListPage{Rows: rows, NextCheckpoint: next, HeadSeq: s.seq, Incarnation: "stub-1"}, nil
}

var _ connector.Connector = (*Stub)(nil)
