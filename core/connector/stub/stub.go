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
	"strings"
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
	state   State
	digest  string
	order   *connector.Order
	seq     int64
	deleted bool   // the order was deleted at the destination (po_deleted)
	event   string // latest journal event for the order (po_edited, po_cancelled)
}

// memProof is the default in-memory restore-proof record. It survives
// Restore, like the real anchor's destination stream.
type memProof struct {
	m map[string]*connector.Proof
}

func (p *memProof) get(op string) *connector.Proof {
	if p.m[op] == nil {
		p.m[op] = &connector.Proof{}
	}
	return p.m[op]
}

func (p *memProof) Proof(_ context.Context, op string) (connector.Proof, error) {
	if v := p.m[op]; v != nil {
		return *v, nil
	}
	return connector.Proof{}, nil
}
func (p *memProof) RecordCreateIntent(_ context.Context, op, _ string) error {
	p.get(op).CreateIntent = true
	return nil
}
func (p *memProof) RecordBlock(_ context.Context, op, _ string) error {
	p.get(op).Blocked = true
	return nil
}
func (p *memProof) RecordCreateDone(_ context.Context, op string, o connector.Order) error {
	if p.get(op).CreateDone == nil {
		p.get(op).CreateDone = &o
	}
	return nil
}

// Stub is safe for concurrent use. The zero value is not usable; call New.
type Stub struct {
	mu          sync.Mutex
	ops         map[string]*record
	seq         int64
	nextOrder   int
	supplierRev map[string]int64
	deliveryRev map[string]int64  // D1 v1.4: protected delivery address revisions (default 1)
	unmapped    map[string]bool   // delivery refs the destination does not know
	taxBP       map[string]int64  // D1 v1.4: tax ref -> rate in basis points
	itemTaxes   map[string]string // D1 v1.4: item ref -> the purchase taxes it requires (joined, sorted)
	dropReply   map[string]int    // op_ref -> execute replies to drop after the create applies
	unreachable map[string]int    // op_ref -> calls that fail before reaching the destination
	creates     int
	proof       connector.RestoreProof

	anchorDown     bool           // restore-proof record unreadable and unwritable
	failCommit     map[string]int // op_ref -> creates whose destination commit fails after CREATE_INTENT
	failBlock      map[string]int // op_ref -> fences whose BLOCK write fails
	loseCreateDone map[string]int // op_ref -> creates whose best-effort CREATE_DONE write is lost

	outOfScope map[string]string   // op_ref -> scope reason the module refuses with (D1 v1.3)
	notReady   string              // readiness reason to report, if set
	foreign    []connector.ListRow // orders created outside Steelmatter
}

// Snapshot is a destination backup: the in-Odoo state only.
type Snapshot struct {
	ops       map[string]record
	seq       int64
	nextOrder int
}

// New returns an empty stub destination with an in-memory restore-proof
// record.
func New() *Stub {
	return NewWithProof(&memProof{m: map[string]*connector.Proof{}})
}

// NewWithProof returns an empty stub destination whose restore-proof record
// is p, for example the real anchor's destination stream (anchor.DestStream),
// as Kaif's Odoo module will use it.
func NewWithProof(p connector.RestoreProof) *Stub {
	return &Stub{ops: map[string]*record{}, supplierRev: map[string]int64{}, deliveryRev: map[string]int64{}, unmapped: map[string]bool{}, taxBP: map[string]int64{},
		itemTaxes: map[string]string{}, dropReply: map[string]int{}, unreachable: map[string]int{},
		proof: p, failCommit: map[string]int{}, failBlock: map[string]int{}, loseCreateDone: map[string]int{}, outOfScope: map[string]string{}}
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

// SupplierRevision implements connector.SupplierSource: the revision set
// with SetSupplierRevision, or MAPPING_MISSING.
func (s *Stub) SupplierRevision(_ context.Context, supplierRef string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rev, ok := s.supplierRev[supplierRef]; ok {
		return rev, nil
	}
	return 0, fmt.Errorf("stub: supplier %q: %s", supplierRef, connector.ReasonMappingMissing)
}

var _ connector.SupplierSource = (*Stub)(nil)

// SetSupplierRevision sets the destination's protected supplier revision.
func (s *Stub) SetSupplierRevision(supplierRef string, rev int64) {
	s.mu.Lock()
	s.supplierRev[supplierRef] = rev
	s.mu.Unlock()
}

// DeliveryRevision implements connector.DeliverySource: the revision set
// with SetDeliveryRevision, or MAPPING_MISSING.
func (s *Stub) DeliveryRevision(_ context.Context, deliveryRef string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unmapped[deliveryRef] {
		return 0, fmt.Errorf("stub: delivery address %q: %s", deliveryRef, connector.ReasonMappingMissing)
	}
	if rev, ok := s.deliveryRev[deliveryRef]; ok {
		return rev, nil
	}
	return 1, nil // like Odoo: every delivery partner starts at revision 1
}

func (s *Stub) curDelivery(ref string) int64 {
	if rev, ok := s.deliveryRev[ref]; ok {
		return rev
	}
	return 1
}

// SetDeliveryUnmapped makes the destination know no delivery address with
// this ref (MAPPING_MISSING).
func (s *Stub) SetDeliveryUnmapped(deliveryRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unmapped[deliveryRef] = true
}

var _ connector.DeliverySource = (*Stub)(nil)

// SetDeliveryRevision sets the destination's protected delivery address revision.
func (s *Stub) SetDeliveryRevision(deliveryRef string, rev int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveryRev[deliveryRef] = rev
}

// SetTax defines a tax the destination knows, with its rate in basis points
// (1800 = 18%). Line taxes are computed per line and rounded half up.
func (s *Stub) SetTax(taxRef string, basisPoints int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taxBP[taxRef] = basisPoints
}

// SetItemTaxes makes the destination require exactly these purchase taxes on
// the item, like an Odoo product with configured taxes (D1 v1.4: an order
// that leaves them out is refused, never created with the tax removed).
func (s *Stub) SetItemTaxes(itemRef string, taxRefs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.itemTaxes[itemRef] = strings.Join(taxRefs, ",")
}

// taxCheck mirrors the module's _check_action and _check_totals for D1 v1.4:
// every tax must be known, each item's required taxes must be authorised
// exactly, and the computed tax must equal the authorised one. It returns
// the refusal reason, or "".
func (s *Stub) taxCheck(a connector.Action) (reason string, untaxed, tax int64) {
	if (a.AmountUntaxedMinor == nil) != (a.TaxMinor == nil) {
		return "TAX_AMOUNTS_INCOMPLETE", 0, 0
	}
	for _, l := range a.Lines {
		if want, ok := s.itemTaxes[l.ItemRef]; ok && want != strings.Join(l.TaxRefs, ",") {
			return "TAX_NOT_AUTHORISED", 0, 0
		}
		lineUntaxed := l.Quantity * l.UnitPriceMinor
		untaxed += lineUntaxed
		for _, t := range l.TaxRefs {
			bp, ok := s.taxBP[t]
			if !ok {
				return connector.ReasonMappingMissing, 0, 0
			}
			tax += (lineUntaxed*bp + 5000) / 10000
		}
	}
	if a.AmountUntaxedMinor == nil {
		if tax != 0 {
			return "TAX_NOT_AUTHORISED", 0, 0
		}
	} else if *a.AmountUntaxedMinor != untaxed || *a.TaxMinor != tax {
		return "TAX_MISMATCH", 0, 0
	}
	if untaxed+tax != a.AmountMinor {
		return "TOTAL_MISMATCH", 0, 0
	}
	return "", untaxed, tax
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
	return s.create(context.Background(), a)
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

// proofOf reads the restore-proof record; ok is false when it is
// unavailable, and then every flag is unknown.
func (s *Stub) proofOf(ctx context.Context, opRef string) (connector.Proof, bool) {
	if s.anchorDown {
		return connector.Proof{}, false
	}
	p, err := s.proof.Proof(ctx, opRef)
	return p, err == nil
}

func (s *Stub) result(ctx context.Context, opRef, call string, o connector.Outcome) connector.Result {
	r := connector.Result{OpRef: opRef, Call: call, Outcome: o, Seq: s.seq, Incarnation: "stub-1",
		OdooTxTime: time.Now().UTC(), ConnectorReceivedAt: time.Now().UTC(), Source: "stub/executor"}
	rec := s.ops[opRef]
	if p, ok := s.proofOf(ctx, opRef); ok {
		r.BlockCommitted, r.PriorCreate = connector.Flag(p.Blocked), connector.Flag(p.CreateIntent)
		if p.CreateDone != nil && (rec == nil || rec.order == nil) {
			// The order was erased (restore); the CREATE_DONE record still
			// establishes it.
			o := *p.CreateDone
			o.Lines = append([]connector.Line(nil), p.CreateDone.Lines...)
			r.CreateDone, r.Destination = true, &o
			r.Reason = "ORDER_ERASED_CREATE_DONE"
		}
	}
	if rec != nil {
		r.Seq = rec.seq
		if rec.order != nil {
			o := *rec.order
			o.Lines = append([]connector.Line(nil), rec.order.Lines...)
			r.Destination = &o
		}
	}
	return r
}

func unavailable(opRef, call, reason string, seq int64) connector.Result {
	return connector.Result{OpRef: opRef, Call: call, Outcome: connector.Unavailable, Reason: reason, Seq: seq}
}

func (s *Stub) failing(opRef string) bool {
	if s.unreachable[opRef] > 0 {
		s.unreachable[opRef]--
		return true
	}
	return false
}

// create runs under s.mu: the per-op_ref lock of the real module.
func (s *Stub) create(ctx context.Context, a connector.Action) connector.Result {
	if a.ExpectedMinSeq > s.seq {
		return s.result(ctx, a.OpRef, "execute", connector.RefusedRestoreSuspected)
	}
	// The block check needs the restore-proof record; if it cannot be read
	// the create is refused (fail closed) and nothing is written.
	p, ok := s.proofOf(ctx, a.OpRef)
	if !ok {
		return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
	}
	// The restore-proof block is checked first, so a delayed create cannot
	// slip through after a restore removed the in-Odoo tombstone, whatever
	// sequence number it carries (D5 v3 case 2).
	if p.Blocked {
		if rec := s.ops[a.OpRef]; rec == nil {
			s.ops[a.OpRef] = &record{state: Fenced, seq: s.seq}
		}
		return s.result(ctx, a.OpRef, "execute", connector.RefusedFenced)
	}
	rec := s.ops[a.OpRef]
	if rec != nil {
		switch rec.state {
		case Fenced, Refused:
			return s.result(ctx, a.OpRef, "execute", connector.RefusedFenced)
		case Created:
			if rec.digest == a.PermitDigest {
				return s.result(ctx, a.OpRef, "execute", connector.Exists)
			}
			return s.result(ctx, a.OpRef, "execute", connector.Mismatch)
		}
	}
	// A create intent with no destination row means an earlier attempt was
	// erased by a restore (or its commit failed). Never create again for this
	// op_ref: block it and refuse (Kaif's case E fix).
	if p.CreateIntent {
		if err := s.proof.RecordBlock(ctx, a.OpRef, "prior attempt"); err != nil {
			return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
		}
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		return s.result(ctx, a.OpRef, "execute", connector.RefusedPriorAttempt)
	}
	// Refusals decided before CREATE_INTENT write BLOCK only, so they carry
	// prior_create = false and can release: out of scope (D1 v1.3), then a
	// stale supplier, in the module's order.
	if reason, ok := s.outOfScope[a.OpRef]; ok {
		if err := s.proof.RecordBlock(ctx, a.OpRef, "out of scope: "+reason); err != nil {
			return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
		}
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		r := s.result(ctx, a.OpRef, "execute", connector.RefusedOutOfScope)
		r.Reason = reason
		return r
	}
	if want, ok := s.supplierRev[a.SupplierRef]; ok && want != a.SupplierRevision {
		if err := s.proof.RecordBlock(ctx, a.OpRef, "supplier stale"); err != nil { // block recorded before the refusal is reported
			return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
		}
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		return s.result(ctx, a.OpRef, "execute", connector.RefusedSupplierStale)
	}
	if a.DeliveryRevision != nil && s.curDelivery(a.DeliveryRef) != *a.DeliveryRevision {
		if err := s.proof.RecordBlock(ctx, a.OpRef, "delivery address stale"); err != nil {
			return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
		}
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		r := s.result(ctx, a.OpRef, "execute", connector.RefusedSupplierStale)
		r.Reason = connector.ReasonDeliveryChanged
		return r
	}
	reason, untaxed, tax := s.taxCheck(a)
	if reason != "" {
		if err := s.proof.RecordBlock(ctx, a.OpRef, "out of scope: "+reason); err != nil {
			return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
		}
		s.seq++
		s.ops[a.OpRef] = &record{state: Refused, digest: a.PermitDigest, seq: s.seq}
		r := s.result(ctx, a.OpRef, "execute", connector.RefusedOutOfScope)
		r.Reason = reason
		return r
	}
	var deliveryRev int64
	if a.DeliveryRevision != nil {
		deliveryRev = *a.DeliveryRevision
	} else {
		deliveryRev = s.curDelivery(a.DeliveryRef)
	}
	// Written before the create commits. If it fails the create rolls back
	// and the answer is UNAVAILABLE, never "not sent": it may have landed.
	if err := s.proof.RecordCreateIntent(ctx, a.OpRef, a.PermitDigest); err != nil {
		return unavailable(a.OpRef, "execute", "ANCHOR_UNAVAILABLE", s.seq)
	}
	if take(s.failCommit, a.OpRef) {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "COMMIT_FAILED", Seq: s.seq}
	}
	s.seq++
	s.nextOrder++
	s.creates++
	order := &connector.Order{
		ID: fmt.Sprintf("PO%05d", s.nextOrder), State: connector.StateConfirmed, CompanyRef: a.CompanyRef, DeliveryRef: a.DeliveryRef,
		SupplierRef: a.SupplierRef, SupplierRevision: a.SupplierRevision, Lines: append([]connector.Line(nil), a.Lines...), AmountMinor: a.AmountMinor, Currency: a.Currency,
		PermitDigest: a.PermitDigest, AmountUntaxedMinor: untaxed, TaxMinor: tax, DeliveryRevision: deliveryRev,
	}
	s.ops[a.OpRef] = &record{state: Created, digest: a.PermitDigest, seq: s.seq, order: order}
	if !take(s.loseCreateDone, a.OpRef) {
		o := *order
		o.Lines = append([]connector.Line(nil), a.Lines...)
		_ = s.proof.RecordCreateDone(ctx, a.OpRef, o) // best-effort
	}
	return s.result(ctx, a.OpRef, "execute", connector.Created)
}

// Execute implements connector.Connector.
func (s *Stub) Execute(ctx context.Context, a connector.Action) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(a.OpRef) {
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	r := s.create(ctx, a)
	if s.dropReply[a.OpRef] > 0 {
		s.dropReply[a.OpRef]--
		return connector.Result{OpRef: a.OpRef, Call: "execute", Outcome: connector.Unknown, Reason: "REPLY_LOST"}, nil
	}
	return r, nil
}

// Lookup implements connector.Connector.
func (s *Stub) Lookup(ctx context.Context, opRef string) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(opRef) {
		return connector.Result{OpRef: opRef, Call: "lookup", Outcome: connector.Unavailable, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	rec := s.ops[opRef]
	if rec == nil {
		if p, ok := s.proofOf(ctx, opRef); ok && p.CreateDone != nil {
			return s.result(ctx, opRef, "lookup", connector.Found), nil // order erased; CREATE_DONE establishes it
		}
		return s.result(ctx, opRef, "lookup", connector.NotFoundNow), nil
	}
	switch {
	case rec.state == Fenced || rec.state == Refused:
		return s.result(ctx, opRef, "lookup", connector.Fenced), nil
	case rec.deleted:
		r := s.result(ctx, opRef, "lookup", connector.Mismatch)
		r.Reason = "ORDER_DELETED"
		return r, nil
	case rec.order.State != connector.StateConfirmed:
		return s.result(ctx, opRef, "lookup", connector.Mismatch), nil
	default:
		return s.result(ctx, opRef, "lookup", connector.Found), nil
	}
}

// Fence implements connector.Connector.
func (s *Stub) Fence(ctx context.Context, opRef string, expectedMinSeq int64) (connector.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing(opRef) {
		return connector.Result{OpRef: opRef, Call: "fence", Outcome: connector.Unknown, Reason: "DESTINATION_UNAVAILABLE"}, nil
	}
	if expectedMinSeq > s.seq {
		return s.result(ctx, opRef, "fence", connector.RefusedRestoreSuspected), nil
	}
	p, ok := s.proofOf(ctx, opRef)
	if !ok {
		return unavailable(opRef, "fence", "ANCHOR_UNAVAILABLE", s.seq), nil
	}
	rec := s.ops[opRef]
	switch {
	case rec == nil && take(s.failBlock, opRef):
		// BLOCK write failed: the fence transaction rolls back, nothing is
		// blocked, and the flag says so.
		r := s.result(ctx, opRef, "fence", connector.Unavailable)
		r.Reason, r.BlockCommitted = "BLOCK_WRITE_FAILED", connector.Flag(false)
		return r, nil
	case rec == nil && p.CreateDone != nil:
		// Order erased by a restore but CREATE_DONE establishes it: block
		// any re-create and report the order (case E).
		if err := s.proof.RecordBlock(ctx, opRef, "fenced"); err != nil {
			return unavailable(opRef, "fence", "ANCHOR_UNAVAILABLE", s.seq), nil
		}
		s.seq++
		s.ops[opRef] = &record{state: Fenced, seq: s.seq}
		return s.result(ctx, opRef, "fence", connector.Exists), nil
	case rec == nil:
		if err := s.proof.RecordBlock(ctx, opRef, "fenced"); err != nil { // block recorded before FENCED is reported
			r := unavailable(opRef, "fence", "BLOCK_WRITE_FAILED", s.seq)
			r.BlockCommitted = connector.Flag(false)
			return r, nil
		}
		s.seq++
		s.ops[opRef] = &record{state: Fenced, seq: s.seq}
		return s.result(ctx, opRef, "fence", connector.Fenced), nil
	case rec.state == Created:
		r := s.result(ctx, opRef, "fence", connector.Exists)
		if rec.deleted {
			r.Reason = "ORDER_DELETED"
		}
		return r, nil
	default:
		if err := s.proof.RecordBlock(ctx, opRef, "fenced"); err != nil {
			return unavailable(opRef, "fence", "ANCHOR_UNAVAILABLE", s.seq), nil
		}
		return s.result(ctx, opRef, "fence", connector.Fenced), nil
	}
}

// DeleteOrder deletes the order created for opRef at the destination, as an
// Odoo administrator can: the operation row stays (so the op_ref can never
// be created again) and the journal records po_deleted.
func (s *Stub) DeleteOrder(opRef string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ops[opRef]
	if rec == nil || rec.state != Created || rec.order == nil {
		return false
	}
	s.seq++
	rec.order, rec.deleted, rec.seq = nil, true, s.seq
	return true
}

// EditOrder changes the committed order for opRef as an Odoo administrator
// can (the journal records po_edited).
func (s *Stub) EditOrder(opRef string, change func(*connector.Order)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ops[opRef]
	if rec == nil || rec.state != Created || rec.order == nil {
		return false
	}
	change(rec.order)
	s.seq++
	rec.event, rec.seq = "po_edited", s.seq
	return true
}

// CancelOrder cancels the committed order for opRef (po_cancelled).
func (s *Stub) CancelOrder(opRef string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ops[opRef]
	if rec == nil || rec.state != Created || rec.order == nil {
		return false
	}
	rec.order.State = "cancel"
	s.seq++
	rec.event, rec.seq = "po_cancelled", s.seq
	return true
}

// SetOutOfScope makes the module refuse opRef as out of its configured scope
// with reason (for example CURRENCY_OUT_OF_SCOPE), as Kaif's module does
// before writing CREATE_INTENT.
func (s *Stub) SetOutOfScope(opRef, reason string) {
	s.mu.Lock()
	s.outOfScope[opRef] = reason
	s.mu.Unlock()
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
		} else if a.DeliveryRevision != nil && s.curDelivery(a.DeliveryRef) != *a.DeliveryRevision {
			r.Outcome, r.Reason = connector.NotReady, connector.ReasonDeliveryChanged
		} else if reason, _, _ := s.taxCheck(a); reason != "" {
			r.Outcome, r.Reason = connector.NotReady, reason
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
				if !o.Lines[i].Equal(a.Lines[i]) {
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
		row.Event = rec.event
		if rec.deleted {
			row.Event = "po_deleted"
		}
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
