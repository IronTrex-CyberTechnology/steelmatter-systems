//go:build integration && failpoints

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// Tests for the owner's R1 decisions of 29 Oct 2026 (docs/r1-decisions.md).

// expireMandate moves a mandate's expiry into the past. Expiry is a signed
// field, so only an administrator with triggers off can model "time passed".
func (e *env) expireMandate(mandate string) {
	e.adminBypass(`UPDATE steelmatter.mandates SET expires_at = issued_at + interval '1 microsecond' WHERE mandate_id = $1`, mandate)
}

// Decision 8: a mandate that expires after dispatch never sends. The worker
// only looks up and fences the operation; nothing reaches the destination.
func TestR1ExpiredMandateNeverSends(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	p := e.permitted()
	mustDispatch(t, e, p)
	e.expireMandate(p.mandate)
	e.retryNow()
	if _, err := e.eng.SettleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Creates() != 0 {
		t.Fatalf("sent a create after mandate expiry (%d orders)", s.Creates())
	}
	if st := e.dispatchStatus(p.operation); st != engine.DispatchFenced {
		t.Fatalf("status %s", st)
	}
	requireBuckets(t, "released by the fence", e.buckets(p.mandate), r1Released)
}

// Decision 8: an attempt already sent before expiry keeps its budget until the
// destination answers. A lost reply stays UNRESOLVED (money held), and the
// later lookup commits the order rather than releasing it.
func TestR1ExpiryKeepsBudgetOfSentAttempt(t *testing.T) {
	e := newR1Env(t)
	s := stub.New()
	e.eng.Connector = s
	p := e.permitted()
	mustDispatch(t, e, p)
	s.DropNextReplies(p.operation, 1)
	requireExecution(t, "lost reply", e.settle(p.operation, s), engine.DispatchUnresolved)
	e.expireMandate(p.mandate)
	requireBuckets(t, "held after expiry", e.buckets(p.mandate), r1Unresolved)
	requireExecution(t, "lookup after expiry", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)
	if s.Creates() != 1 {
		t.Fatalf("orders %d", s.Creates())
	}
}

// heldNeverSent makes a HELD operation whose anchor shows no create attempt.
// Today's settle path never produces one (every HELD has a create attempt), so
// the status is set directly; it models the only case decision 3 can release.
func (e *env) heldNeverSent(op string, s *stub.Stub) {
	e.t.Helper()
	s.FailNextCalls(op, 1) // the execute never reaches the destination
	requireExecution(e.t, "never reached", e.settle(op, s), engine.DispatchUnresolved)
	e.adminBypass(`UPDATE steelmatter.dispatches SET status = 'HELD' WHERE op_ref = $1`, op)
}

func requireFault(t *testing.T, what string, err error, code, contains string) {
	t.Helper()
	f, ok := engine.AsFault(err)
	if !ok || f.Code != code || !strings.Contains(err.Error(), contains) {
		t.Fatalf("%s: got %v, want %s containing %q", what, err, code, contains)
	}
}

// Decision 3 (R1-DEF-08): a HELD operation whose anchor records a create
// attempt is never released, even with a finding, a review and Aakhil's
// confirmation. The repair is refused at preparation; COMMIT stays possible.
func TestR1HeldWithCreateAttemptIsNeverReleased(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	mustDispatch(t, e, p)
	e.heldVia(p.operation, a.s)
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "supplier says no order", "ticket-9"); err != nil {
		t.Fatal(err)
	}
	_, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairRelease, "supplier-email.pdf", "no order received")
	requireFault(t, "prepare RELEASE", err, "FORBIDDEN", "RELEASE_NOT_PROVEN")
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
	if _, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairCommit, "supplier-email.pdf", "order confirmed"); err != nil {
		t.Fatalf("COMMIT must stay possible: %v", err)
	}
}

// Decision 3: the proof is checked again at confirmation. A create attempt
// recorded after the repair was prepared blocks the release.
func TestR1ReleaseRecheckedAtConfirmation(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	mustDispatch(t, e, p)
	e.heldNeverSent(p.operation, a.s)
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "never reached Odoo", "ticket-10"); err != nil {
		t.Fatal(err)
	}
	id, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairRelease, "logs", "never sent")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "ok"); err != nil {
		t.Fatal(err)
	}
	ds := &anchor.DestStream{Pool: a.ah.Dest, Destination: e.eng.Cfg.CompanyRef}
	if err := ds.RecordCreateIntent(ctx, p.operation, "late"); err != nil {
		t.Fatal(err)
	}
	_, err = e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s)
	requireFault(t, "confirm RELEASE", err, "FORBIDDEN", "RELEASE_NOT_PROVEN")
	requireBuckets(t, "still held", e.buckets(p.mandate), r1Unresolved)
	if a.count(anchor.RepairConfirmed) != 0 {
		t.Fatal("a refused release was recorded as confirmed")
	}
}

// Decision 3: a release fences the reference first; after that the anchor
// refuses any create for it, so a late request can never create an order.
func TestR1ReleaseBlocksReferencePermanently(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	ctx := context.Background()
	mustDispatch(t, e, p)
	e.heldNeverSent(p.operation, a.s)
	if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "never reached Odoo", "ticket-11"); err != nil {
		t.Fatal(err)
	}
	id, _ := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairRelease, "logs", "never sent")
	if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "ok"); err != nil {
		t.Fatal(err)
	}
	if d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err != nil || d.Execution != engine.DispatchFenced {
		t.Fatalf("confirm: %+v %v", d, err)
	}
	requireBuckets(t, "released", e.buckets(p.mandate), r1Released)
	ds := &anchor.DestStream{Pool: a.ah.Dest, Destination: e.eng.Cfg.CompanyRef}
	if err := ds.RecordCreateIntent(ctx, p.operation, "late"); !errors.Is(err, anchor.ErrBlocked) {
		t.Fatalf("create intent after release: %v", err)
	}
	if r := a.s.Deliver(stubActionFor(t, e, p)); r.Outcome != connector.RefusedFenced || a.s.Creates() != 0 {
		t.Fatalf("late create after release: %s, %d orders", r.Outcome, a.s.Creates())
	}
}

// anchorRepair runs the three-person anchor repair (decision 4): Uday
// prepares, Kaif reviews, Aakhil confirms.
func (e *env) anchorRepair(op, evidence string) error {
	ctx := context.Background()
	id, err := e.eng.PrepareRepair(ctx, uday, op, engine.RepairAnchor, evidence, "anchor behind the engine")
	if err != nil {
		return err
	}
	if err := e.eng.ReviewRepair(ctx, kaif, op, id, true, "checked"); err != nil {
		return err
	}
	return e.eng.ConfirmAnchorRepair(ctx, aakhil, op, id)
}

// Decision 4 over HTTP: the anchor repair takes a prepared and reviewed
// repair_id; an evidence reference alone is no longer accepted.
func TestR1HTTPAnchorRepairNeedsThreePeople(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	mustDispatch(t, e, p) // before the anchor was attached
	a := withAnchor(t, e)
	e.httpOperators()
	path := "/v1/dispatches/" + p.operation
	st, body := e.call("POST", path+"/anchor-repair", "aakhil", map[string]any{"evidence_ref": "engine-export"})
	requireStatus(t, "old body", st, body, 400, "UNKNOWN_FIELD")
	st, body = e.call("POST", path+"/repairs", "uday", map[string]any{"outcome": "ANCHOR_REPAIR", "evidence_ref": "engine-export"})
	requireStatus(t, "prepare", st, body, 201, "")
	id, _ := body["repair_id"].(string)
	st, body = e.call("POST", path+"/anchor-repair", "aakhil", map[string]any{"repair_id": id})
	requireStatus(t, "confirm without review", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", path+"/repairs/"+id+"/review", "kaif", map[string]any{"approved": true})
	requireStatus(t, "review", st, body, 200, "")
	st, body = e.call("POST", path+"/repairs/"+id+"/confirm", "aakhil", nil)
	requireStatus(t, "budget confirm of an anchor repair", st, body, 409, "REVISION_STALE")
	st, body = e.call("POST", path+"/anchor-repair", "aakhil", map[string]any{"repair_id": id})
	requireStatus(t, "confirm", st, body, 200, "")
	if body["repaired"] != true || body["repair_id"] != id {
		t.Fatalf("anchor repair %v", body)
	}
	if a.count(anchor.DispatchIntent) != 1 {
		t.Fatal("intent not re-appended")
	}
	// New spending on the same mandate is allowed again: the anchor repair is
	// not a budget decision the engine still has to apply.
	inst := e.instruction("po-after-anchor-repair", p.mandate, line("item-a", 1, 1_000_000))
	st, body = e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate after the anchor repair", st, body, 201, "")
	a.verify()
}

// ---- Decisions 1 and 2: D1 v1.4 (R1-DEF-11, R1-DEF-21, R1-DEF-22) ----

const (
	untaxedSix = int64(60_000_000)
	gst18      = int64(10_800_000) // 18% of 60,000,000
	taxedTotal = untaxedSix + gst18
)

// taxedDestination is a stub destination with an 18% GST that item-a
// requires, and a protected delivery address revision 4.
func taxedDestination(e *env) *stub.Stub {
	s := stub.New()
	s.SetTax("gst-18", 1800)
	s.SetItemTaxes("item-a", "gst-18")
	s.SetDeliveryRevision("warehouse-a", 4)
	e.eng.Connector = s
	return s
}

func taxedLine(tax ...string) map[string]any {
	l := line("item-a", 3, 20_000_000)
	l["unit_id"] = "unit-each"
	if len(tax) > 0 {
		l["tax_ids"] = tax
	}
	return l
}

func taxedInstruction(record, mandate string, tax *int64, lines ...map[string]any) map[string]any {
	b := instructionBody(record, mandate, lines...)
	if tax != nil {
		b["proposed_action"].(map[string]any)["tax_minor"] = *tax
	}
	return b
}

func i64(v int64) *int64 { return &v }

func (e *env) taxedPermitted(tax *int64, lines ...map[string]any) permitted {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	st, inst := e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-taxed-001", m, tax, lines...))
	requireStatus(e.t, "create instruction", st, inst, 201, "")
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(e.t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	return permitted{mandate: m, instruction: inst["instruction_id"].(string), action: body["action"].(map[string]any),
		jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
}

// Decision 1: the exact quantities, units, taxes and the full total are
// authorised and held, sent to the destination as authorised and verified
// on the order it created.
func TestR1TaxedOrderHoldsFullTotalAndCommits(t *testing.T) {
	e := newR1Env(t)
	s := taxedDestination(e)
	p := e.taxedPermitted(i64(gst18), taxedLine("gst-18"))
	a := p.action
	if fmt.Sprint(a["amount_minor"]) != fmt.Sprint(taxedTotal) || fmt.Sprint(a["amount_untaxed_minor"]) != fmt.Sprint(untaxedSix) || fmt.Sprint(a["tax_minor"]) != fmt.Sprint(gst18) ||
		fmt.Sprint(a["delivery_revision"]) != fmt.Sprint(4) {
		t.Fatalf("authorised action %v", a)
	}
	l := a["lines"].([]any)[0].(map[string]any)
	if l["unit_id"] != "unit-each" || len(l["tax_ids"].([]any)) != 1 {
		t.Fatalf("authorised line %v", l)
	}
	held := buckets{Available: limit - taxedTotal, Unresolved: taxedTotal}
	mustDispatch(t, e, p)
	requireBuckets(t, "full total held", e.buckets(p.mandate), held)
	requireExecution(t, "settle", e.settle(p.operation, s), engine.DispatchCommitted)
	requireBuckets(t, "full total committed", e.buckets(p.mandate), buckets{Available: limit - taxedTotal, Committed: taxedTotal})
	if s.Creates() != 1 {
		t.Fatalf("orders %d", s.Creates())
	}
}

// Decision 1: the engine never removes tax or invents a tax amount. Taxes
// without a tax amount, or a tax amount without taxes, are refused.
func TestR1TaxFieldsMustComeTogether(t *testing.T) {
	e := newR1Env(t)
	taxedDestination(e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	st, body := e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-tax-1", m, nil, taxedLine("gst-18")))
	requireStatus(t, "taxes without tax_minor", st, body, 400, "SCHEMA_VIOLATION")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-tax-2", m, i64(5), taxedLine()))
	requireStatus(t, "tax_minor without taxes", st, body, 400, "SCHEMA_VIOLATION")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-tax-3", m, i64(1), taxedLine("gst-18", "gst-18")))
	requireStatus(t, "repeated tax", st, body, 400, "SCHEMA_VIOLATION")
	l := taxedLine("gst-18")
	l["uom"] = "kg"
	st, body = e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-tax-4", m, i64(1), l))
	requireStatus(t, "unknown line field", st, body, 400, "UNKNOWN_FIELD")
}

// Decision 1: an unsupported configuration is refused, never changed. A
// product whose purchase tax was left out, or a tax amount the destination
// computes differently, is not dispatched and nothing is created.
func TestR1UnsupportedTaxConfigurationIsRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		tax    *int64
		line   map[string]any
		reason string
	}{
		{"tax left out", nil, taxedLine(), "TAX_NOT_AUTHORISED"},
		{"tax amount differs", i64(gst18 - 1), taxedLine("gst-18"), "TAX_MISMATCH"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newR1Env(t)
			s := taxedDestination(e)
			p := e.taxedPermitted(c.tax, c.line)
			_, err := e.dispatch("agent-a", p)
			requireFault(t, "dispatch", err, "ADAPTER_UNSUPPORTED", c.reason)
			if s.Creates() != 0 || e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 {
				t.Fatal("an unsupported configuration was dispatched")
			}
		})
	}
}

// Decision 1 at the destination itself: if the configuration changes after
// dispatch, the create is refused before any create attempt and the money is
// released; the order is never created with the tax removed.
func TestR1TaxRefusalAtCreateReleases(t *testing.T) {
	e := newR1Env(t)
	s := taxedDestination(e)
	p := e.taxedPermitted(i64(gst18), taxedLine("gst-18"))
	mustDispatch(t, e, p)
	s.SetTax("gst-18", 2800) // the tax rate changed in Odoo after dispatch
	requireExecution(t, "settle", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(p.mandate), buckets{Available: limit})
	if s.Creates() != 0 {
		t.Fatal("created an order with a different tax")
	}
}

// Decision 2: a delivery address changed before the purchase is created
// needs a fresh authorisation: at evaluation, at dispatch, and inside the
// destination create.
func TestR1DeliveryChangeNeedsFreshAuthorisation(t *testing.T) {
	e := newR1Env(t)
	s := taxedDestination(e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	st, inst := e.call("POST", "/v1/instructions", "issuer-a", taxedInstruction("po-del-1", m, i64(gst18), taxedLine("gst-18")))
	requireStatus(t, "create", st, inst, 201, "")
	id := inst["instruction_id"].(string)
	s.SetDeliveryRevision("warehouse-a", 5)
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate after the address changed", st, body, 409, "DELIVERY_REVISION_STALE")
	requireBuckets(t, "nothing reserved", e.buckets(m), buckets{Available: limit})

	st, body = e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", map[string]any{"amendment_id": "amd-del-1", "expected_action_revision": 1,
		"proposed_action": taxedInstruction("po-del-1", m, i64(gst18), taxedLine("gst-18"))["proposed_action"]})
	requireStatus(t, "amend", st, body, 201, "")
	if fmt.Sprint(body["action"].(map[string]any)["delivery_revision"]) != fmt.Sprint(5) {
		t.Fatalf("amended action %v", body["action"])
	}
	st, body = e.evaluate("agent-a", id, 2)
	requireStatus(t, "fresh authorisation", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	p := permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}

	s.SetDeliveryRevision("warehouse-a", 6)
	_, err := e.dispatch("agent-a", p)
	requireFault(t, "dispatch after a change", err, "DELIVERY_REVISION_STALE", "delivery")
	s.SetDeliveryRevision("warehouse-a", 5)
	mustDispatch(t, e, p)
	s.SetDeliveryRevision("warehouse-a", 7) // changed between dispatch and create
	requireExecution(t, "create refused", e.settle(p.operation, s), engine.DispatchFenced)
	requireBuckets(t, "released", e.buckets(m), buckets{Available: limit})
	if s.Creates() != 0 {
		t.Fatal("created an order for a changed delivery address")
	}
}

// Decision 2: the protected revisions only move forward; nothing an
// operator or issuer sends can set them. tax_minor is the only tax input,
// and delivery_revision is never accepted from the issuer.
func TestR1DeliveryRevisionIsNotIssuerInput(t *testing.T) {
	e := newR1Env(t)
	taxedDestination(e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	b := taxedInstruction("po-del-2", m, i64(gst18), taxedLine("gst-18"))
	b["proposed_action"].(map[string]any)["delivery_revision"] = 9
	st, body := e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "issuer-sent delivery_revision", st, body, 400, "UNKNOWN_FIELD")
	b = taxedInstruction("po-del-3", m, i64(gst18), taxedLine("gst-18"))
	b["proposed_action"].(map[string]any)["amount_minor"] = 1
	st, body = e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "issuer-sent total", st, body, 400, "UNKNOWN_FIELD")
}

// The settle comparison covers the new facts: an order created with another
// unit or for another delivery address revision is a MISMATCH, never
// committed.
func TestR1OrderWithDifferentUnitOrDeliveryIsMismatch(t *testing.T) {
	for _, c := range []struct {
		name  string
		alter func(s *stub.Stub, a *connector.Action)
	}{
		{"unit", func(_ *stub.Stub, a *connector.Action) { a.Lines[0].UnitRef = "unit-box" }},
		{"delivery revision", func(s *stub.Stub, a *connector.Action) {
			s.SetDeliveryRevision("warehouse-a", 5)
			a.DeliveryRevision = i64(5)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newR1Env(t)
			s := taxedDestination(e)
			p := e.taxedPermitted(i64(gst18), taxedLine("gst-18"))
			mustDispatch(t, e, p)
			act := stubActionFor(t, e, p)
			c.alter(s, &act)
			if r := s.Deliver(act); r.Outcome != connector.Created {
				t.Fatalf("altered order: %s %s", r.Outcome, r.Reason)
			}
			s.SetDeliveryRevision("warehouse-a", 4)
			requireExecution(t, "settle", e.settle(p.operation, s), engine.DispatchMismatch)
			requireBuckets(t, "held", e.buckets(p.mandate), buckets{Available: limit - taxedTotal, Unresolved: taxedTotal})
		})
	}
}

// ---- Decision 6: one tenant per deployment ----

// An R1 deployment serves one tenant: every request of another tenant's
// principals is refused before any endpoint reads or writes (NEW-SEC-01,
// QA 30 Oct 2026): no mandate, instruction, amendment, permit or reservation,
// no dispatch, no recovery or repairs.
func TestR1OneTenantPerDeployment(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	// Another tenant's state that predates the restriction (built while the
	// deployment served any tenant).
	other := e.permittedIn("other-org", "owner-x", "issuer-x", "agent-x", "po-other-r1")
	e.eng.Cfg.Tenant = tenant
	rows := func() int {
		return e.count(`SELECT (SELECT count(*) FROM steelmatter.mandates WHERE tenant_id = 'other-org')
			+ (SELECT count(*) FROM steelmatter.instructions WHERE tenant_id = 'other-org')
			+ (SELECT count(*) FROM steelmatter.action_revisions WHERE tenant_id = 'other-org')
			+ (SELECT count(*) FROM steelmatter.amendments WHERE tenant_id = 'other-org')
			+ (SELECT count(*) FROM steelmatter.issuances WHERE tenant_id = 'other-org')
			+ (SELECT count(*) FROM steelmatter.obligations WHERE tenant_id = 'other-org')`)
	}
	before := rows()
	for _, c := range []struct {
		what, method, path, who string
		body                    map[string]any
	}{
		{"create mandate", "POST", "/v1/mandates", "owner-x", map[string]any{
			"actor_ids": []string{"agent-x"}, "audience": audience, "supplier_ids": []string{"supplier-a"}, "delivery_ids": []string{"warehouse-a"},
			"currency": "INR", "limit_minor": limit, "expires_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")}},
		{"revoke mandate", "POST", "/v1/mandates/" + other.mandate + "/revoke", "owner-x", nil},
		{"create instruction", "POST", "/v1/instructions", "issuer-x", instructionBody("po-other-r1-2", other.mandate)},
		{"read instruction", "GET", "/v1/instructions/" + other.instruction, "issuer-x", nil},
		{"amend instruction", "POST", "/v1/instructions/" + other.instruction + "/revisions", "issuer-x", amendBody("amd-x", 1, line("item-a", 2, 25_000_000))},
		{"evaluate", "POST", "/v1/actions/evaluate", "agent-x", map[string]any{"instruction_id": other.instruction, "action_revision": 1}},
		{"simulate", "POST", "/v1/actions/simulate", "agent-x", map[string]any{"instruction_id": other.instruction, "action_revision": 1}},
		{"admit", "POST", "/v1/operations", "agent-x", map[string]any{"action": other.action, "permit_jws": other.jws}},
		{"dispatch", "POST", "/v1/dispatches", "agent-x", dispatchBody(other)},
	} {
		st, body := e.call(c.method, c.path, c.who, c.body)
		requireStatus(t, "other tenant "+c.what, st, body, 403, "FORBIDDEN")
	}
	if n := rows(); n != before {
		t.Fatalf("another tenant changed engine state: %d rows, was %d", n, before)
	}
	if e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 || a.s.Creates() != 0 {
		t.Fatal("another tenant dispatched")
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	e.admin(`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ('operator-x', 'other-org', 'OPERATOR', $1)`, hash)
	e.tokens["operator-x"] = token
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/recovery/check"}, {"POST", "/v1/recovery/reconcile"}, {"POST", "/v1/recovery/resume"},
		{"GET", "/v1/recovery/write-state"}, {"POST", "/v1/dispatches/op-1/settle"},
	} {
		st, body := e.call(c.method, c.path, "operator-x", nil)
		requireStatus(t, "other tenant "+c.path, st, body, 403, "FORBIDDEN")
	}
	// The served tenant works as before.
	p := e.permitted()
	st, body := e.call("POST", "/v1/dispatches", "agent-a", dispatchBody(p))
	requireStatus(t, "served tenant", st, body, 201, "")
}

// ---- Decision 10: observe-only rollout before purchasing ----

// In observe-only mode nothing is purchased: dispatch is refused before
// anything is consumed, an operation dispatched before the switch is only
// looked up and fenced, and the restore check keeps running.
func TestR1ObserveOnlyNeverPurchases(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	earlier := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-before-switch")
	mustDispatch(t, e, earlier)
	e.eng.Cfg.ObserveOnly = true

	p := e.permitted()
	_, err := e.dispatch("agent-a", p)
	requireFault(t, "dispatch", err, "FORBIDDEN", "OBSERVE_ONLY")
	if e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = $1`, p.operation) != 0 {
		t.Fatal("observe-only recorded a dispatch")
	}
	// Observe-only never writes to the destination, not even a fence
	// (Aakhil, 30 Oct 2026): the earlier operation is only looked up and
	// stays held.
	requireExecution(t, "earlier dispatch", e.settle(earlier.operation, a.s), engine.DispatchPending)
	if a.s.Creates() != 0 || a.s.StateOf(earlier.operation) != stub.Absent {
		t.Fatalf("observe-only wrote to the destination: creates %d, state %q", a.s.Creates(), a.s.StateOf(earlier.operation))
	}
	if r := a.check(); r.WritesDisabled {
		t.Fatalf("restore check in observe-only: %+v", r)
	}
	// A deployment with write authority settles it later (here: sends the
	// never-sent create once).
	e.eng.Cfg.ObserveOnly = false
	requireExecution(t, "settled by a writer", e.settle(earlier.operation, a.s), engine.DispatchCommitted)
	if a.s.Creates() != 1 {
		t.Fatalf("creates %d", a.s.Creates())
	}
}

// Aakhil, 29 Oct 2026: the delivery check must not be optional. With a
// destination configured, an action without a delivery address revision is
// refused at evaluation and at dispatch, before anything is consumed; an
// amendment builds it again with the revision. A delivery address the
// destination does not know is refused when the instruction is created.
func TestR1DeliveryRevisionIsRequired(t *testing.T) {
	e := newEnv(t) // no destination yet: the action is built without a delivery revision
	p := e.permitted()
	if _, ok := p.action["delivery_revision"]; ok {
		t.Fatalf("action built without a destination has a delivery revision: %v", p.action)
	}
	s := stub.New()
	e.eng.Connector = s

	_, err := e.dispatch("agent-a", p)
	requireFault(t, "dispatch without delivery revision", err, "DELIVERY_REVISION_STALE", "no delivery address revision")
	requireBuckets(t, "nothing consumed", e.buckets(p.mandate), r1Reserved)
	if e.count(`SELECT count(*) FROM steelmatter.dispatches`) != 0 || s.Creates() != 0 {
		t.Fatal("an action without a delivery revision was dispatched")
	}
	st, body := e.evaluate("agent-a", p.instruction, 1)
	requireStatus(t, "evaluate without delivery revision", st, body, 409, "DELIVERY_REVISION_STALE")

	st, body = e.call("POST", "/v1/instructions/"+p.instruction+"/revisions", "issuer-a", map[string]any{"amendment_id": "amd-delivery",
		"expected_action_revision": 1, "proposed_action": instructionBody("po-request-001", "")["proposed_action"]})
	requireStatus(t, "amend", st, body, 201, "")
	if fmt.Sprint(body["action"].(map[string]any)["delivery_revision"]) != "1" {
		t.Fatalf("amended action %v", body["action"])
	}
	st, body = e.evaluate("agent-a", p.instruction, 2)
	requireStatus(t, "evaluate the amended action", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	p2 := permitted{mandate: p.mandate, instruction: p.instruction, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
	mustDispatch(t, e, p2)
	requireExecution(t, "settle", e.settle(p2.operation, s), engine.DispatchCommitted)

	// An address the destination does not know is refused up front.
	s.SetDeliveryUnmapped("warehouse-a")
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-unmapped", m))
	requireStatus(t, "unmapped delivery address", st, body, 422, "ADAPTER_UNSUPPORTED")
}
