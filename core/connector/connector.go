// Package connector defines the engine-connector interface agreed as D1
// (Uday and Kaif, 28 Oct 2026; v1.3 adds REFUSED_OUT_OF_SCOPE). The engine owns operation state and every
// allowance move; a connector only reports destination facts and never
// writes to Steelmatter's ledger.
//
// The rule every implementation must keep: a timeout, lost reply, ambiguous
// error or plain "not found" is never proof that no order exists. Only a
// verified order (CREATED, EXISTS, FOUND) or an authoritative block (FENCED,
// or a structured module refusal that writes a tombstone) settles an
// operation.
package connector

import (
	"context"
	"time"
)

// Outcome is a connector result value. Which values a call may return is
// listed on each Connector method.
type Outcome string

const (
	// Execute (sm_create).
	Created                 Outcome = "CREATED"                   // new order created and confirmed in one transaction
	Exists                  Outcome = "EXISTS"                    // same op_ref and digest already created; order returned (also a fence result)
	Mismatch                Outcome = "MISMATCH"                  // same op_ref, different digest; or a draft/differing order on lookup
	RefusedFenced           Outcome = "REFUSED_FENCED"            // op_ref is tombstoned; nothing written
	RefusedSupplierStale    Outcome = "REFUSED_SUPPLIER_STALE"    // supplier revision changed; op_ref tombstoned in the same transaction
	RefusedRestoreSuspected Outcome = "REFUSED_RESTORE_SUSPECTED" // destination head behind expected_min_seq; nothing written
	RefusedPriorAttempt     Outcome = "REFUSED_PRIOR_ATTEMPT"     // restore-proof journal has a create intent but the destination has no row (restored); block written, no create
	RefusedOutOfScope       Outcome = "REFUSED_OUT_OF_SCOPE"      // D1 v1.3: the action is outside the destination's configured scope (mapping, currency, lines, approval mode); decided before CREATE_INTENT, block written, no create
	Unknown                 Outcome = "UNKNOWN"                   // timeout, reset, 5xx, malformed reply, standard destination error

	// Lookup.
	Found       Outcome = "FOUND"         // confirmed order present
	NotFoundNow Outcome = "NOT_FOUND_NOW" // observation at this moment only; never a release
	Unavailable Outcome = "UNAVAILABLE"

	// Fence (sm_fence). Also returns Exists, RefusedRestoreSuspected, Unknown.
	Fenced Outcome = "FENCED" // no order; tombstone written under the same lock as create

	// Readiness: read-only and advisory. NOT_READY blocks dispatch; READY
	// cannot rule out a change before the write.
	Ready    Outcome = "READY"
	NotReady Outcome = "NOT_READY" // Reason says why

	// Observe (read-only observer identity): evidence only, never moves
	// allowance. Also returns Mismatch.
	Match                Outcome = "MATCH"
	InsufficientEvidence Outcome = "INSUFFICIENT_EVIDENCE"
	NotFound             Outcome = "NOT_FOUND"
)

// Readiness reason codes.
const (
	ReasonMappingMissing         = "MAPPING_MISSING"
	ReasonVersionUnsupported     = "VERSION_UNSUPPORTED"
	ReasonCredentialDisabled     = "CREDENTIAL_DISABLED"
	ReasonDestinationUnavailable = "DESTINATION_UNAVAILABLE"
	ReasonSupplierChanged        = "SUPPLIER_CHANGED"
)

// Tombstones reports whether o is a structured module refusal that blocks
// the op_ref permanently. Like FENCED, it releases an amount only when the
// result also shows the block committed durably and no earlier create
// occurred (Result.ProvesNoOrder; Aakhil, 28 Oct 2026).
func (o Outcome) Tombstones() bool {
	return o == RefusedFenced || o == RefusedSupplierStale || o == RefusedPriorAttempt || o == RefusedOutOfScope
}

// Line is one purchase line in minor units.
type Line struct {
	ItemRef        string `json:"item_ref"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

// Action is the canonical action execute sends: only fields the engine
// stored and authorised. No destination models, IDs or commands; the
// connector maps every *Ref through its configured mapping.
//
// SupplierRevision has one definition everywhere: the destination's protected
// supplier revision counter (Odoo res.partner.sm_revision), read through the
// connector when the action is authorised and compared by the destination
// under a supplier row lock inside the create.
type Action struct {
	OpRef            string `json:"op_ref"`
	ActionRevision   int64  `json:"action_revision"`
	CompanyRef       string `json:"company_ref"`
	DeliveryRef      string `json:"delivery_ref"`
	SupplierRef      string `json:"supplier_ref"`
	SupplierRevision int64  `json:"supplier_revision"`
	Lines            []Line `json:"lines"`
	AmountMinor      int64  `json:"amount_minor"`
	Currency         string `json:"currency"`
	PermitDigest     string `json:"permit_digest"`
	ExpectedMinSeq   int64  `json:"expected_min_seq"`
}

// Order is what the destination holds for an op_ref.
type Order struct {
	ID           string `json:"id"`
	State        string `json:"state"` // "purchase" = confirmed; anything else is not FOUND
	CompanyRef   string `json:"company_ref"`
	DeliveryRef  string `json:"delivery_ref"`
	SupplierRef  string `json:"supplier_ref"`
	Lines        []Line `json:"lines"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	PermitDigest string `json:"permit_digest"`
}

// StateConfirmed is the destination order state that counts as success.
const StateConfirmed = "purchase"

// Result is the envelope every call returns.
type Result struct {
	OpRef               string    `json:"op_ref"`
	Call                string    `json:"call"`
	Outcome             Outcome   `json:"outcome"`
	Reason              string    `json:"reason,omitempty"`
	Seq                 int64     `json:"seq"`
	Incarnation         string    `json:"incarnation,omitempty"`
	OdooTxTime          time.Time `json:"odoo_tx_time,omitempty"`
	ConnectorReceivedAt time.Time `json:"connector_received_at,omitempty"`
	Source              string    `json:"source,omitempty"`
	Destination         *Order    `json:"destination,omitempty"`

	// The proof flags are tri-state: nil means unknown (for example the
	// restore-proof record could not be read), and unknown is never treated
	// as false.
	//
	// BlockCommitted: the permanent block on op_ref has committed at the
	// destination and in its restore-proof block record (D5 v3), so no later
	// create can succeed, even after a destination restore.
	BlockCommitted *bool `json:"block_committed"`
	// PriorCreate: the destination's restore-proof journal shows a create was
	// attempted for op_ref before the block. The earlier outcome is then not
	// established, so blocking future attempts cannot release the amount.
	PriorCreate *bool `json:"prior_create"`
	// CreateDone: the destination's best-effort CREATE_DONE record shows the
	// create committed. Destination then carries the order facts from that
	// record, so the earlier outcome is established even if a restore erased
	// the order (case E: committed, flagged, never refunded).
	CreateDone bool `json:"create_done,omitempty"`
}

// Flag returns a known proof-flag value.
func Flag(v bool) *bool { return &v }

func isTrue(b *bool) bool  { return b != nil && *b }
func isFalse(b *bool) bool { return b != nil && !*b }

// ProvesNoOrder reports whether r proves that no order exists or can ever
// exist for op_ref: a FENCED result or a structured module refusal whose
// permanent block is known to have committed, with no earlier create attempt
// known. Unknown flags never prove anything.
func (r Result) ProvesNoOrder() bool {
	return (r.Outcome == Fenced || r.Outcome.Tombstones()) && isTrue(r.BlockCommitted) && isFalse(r.PriorCreate) && !r.CreateDone
}

// BlockedOverPriorCreate reports whether r is a block (FENCED or a module
// refusal) over a known earlier create attempt whose outcome is not
// established: the operation is HELD, never released.
func (r Result) BlockedOverPriorCreate() bool {
	return (r.Outcome == Fenced || r.Outcome.Tombstones()) && isTrue(r.PriorCreate) && !r.CreateDone
}

// ListRow is one destination journal row (sm_list_since): an operation the
// module created, fenced or refused, or a foreign order created outside
// Steelmatter (no op_ref; a coverage gap, informational only).
type ListRow struct {
	Seq          int64  `json:"seq"`
	OpRef        string `json:"op_ref,omitempty"`
	State        string `json:"state"` // created, fenced, refused, foreign
	OrderID      string `json:"order_id,omitempty"`
	AmountMinor  int64  `json:"amount_minor,omitempty"`
	Currency     string `json:"currency,omitempty"`
	PermitDigest string `json:"permit_digest,omitempty"`
	Tombstoned   bool   `json:"tombstoned"`
	Foreign      bool   `json:"foreign,omitempty"`
}

// ListPage is one page of the destination journal, in commit (seq) order.
type ListPage struct {
	Rows           []ListRow `json:"rows"`
	NextCheckpoint int64     `json:"next_checkpoint"`
	HeadSeq        int64     `json:"head_seq"`
	Incarnation    string    `json:"incarnation"`
}

// Connector is the destination side of D1 v1. Implementations must be safe
// for concurrent use. A transport failure is reported as an Unknown result,
// never as an error that the caller could mistake for "nothing happened";
// the error return is reserved for programming faults.
type Connector interface {
	// Execute may return Created, Exists, Mismatch, RefusedFenced,
	// RefusedSupplierStale, RefusedPriorAttempt, RefusedOutOfScope, RefusedRestoreSuspected,
	// Unavailable (restore-proof record unreadable; nothing created) or
	// Unknown.
	Execute(ctx context.Context, a Action) (Result, error)
	// Lookup may return Found, Mismatch, NotFoundNow, Fenced or Unavailable.
	Lookup(ctx context.Context, opRef string) (Result, error)
	// Fence may return Fenced, Exists, RefusedRestoreSuspected, Unavailable
	// (restore-proof record unreadable or the block write failed; nothing
	// blocked) or Unknown.
	Fence(ctx context.Context, opRef string, expectedMinSeq int64) (Result, error)
	// Readiness may return Ready or NotReady (with a reason). Read-only and
	// advisory: it never proves anything about a later write.
	Readiness(ctx context.Context, a Action) (Result, error)
	// Observe compares the destination with the authorised action using the
	// read-only observer identity. It may return Match, Mismatch,
	// InsufficientEvidence or NotFound, and never moves allowance.
	Observe(ctx context.Context, a Action) (Result, error)
	// List returns journal rows with seq > checkpointSeq, at most limit, in
	// commit order, using the read-only observer identity (restore check and
	// reconciliation).
	List(ctx context.Context, checkpointSeq int64, limit int) (ListPage, error)
}

// Proof is what the destination's restore-proof record (the anchor's
// destination stream, D5 v3) holds for one op_ref. It is outside Odoo's
// rollback domain, so it survives an Odoo restore.
type Proof struct {
	CreateIntent bool   // CREATE_INTENT: a create was attempted (written before it commits)
	Blocked      bool   // BLOCK: the op_ref is permanently blocked
	CreateDone   *Order // CREATE_DONE: best-effort, after the create committed; the order facts
}

// RestoreProof is the destination module's access to its restore-proof
// record. Kaif's Odoo module writes it with the steelmatter-dest-writer
// identity; the stub connector uses the same interface. A read or write
// error means the record is unavailable: the module must fail closed.
type RestoreProof interface {
	Proof(ctx context.Context, opRef string) (Proof, error)
	RecordCreateIntent(ctx context.Context, opRef, permitDigest string) error
	RecordBlock(ctx context.Context, opRef, reason string) error
	RecordCreateDone(ctx context.Context, opRef string, o Order) error
}
