//go:build integration && failpoints

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// T17-T19: exact-action human approval (R1 decision 9, migration 0017).
// Policy: actions above ₹5,00,000 need an approval; approver-a is entitled.

const approvalAbove = int64(50_000_000)

func (e *env) approvalPolicy() {
	e.t.Helper()
	e.admin(`INSERT INTO steelmatter.approval_policies (tenant_id, approval_above_minor) VALUES ($1, $2)`, tenant, approvalAbove)
	e.addPrincipal("approver-a", "OPERATOR")
	e.admin(`INSERT INTO steelmatter.approver_entitlements (tenant_id, approver_principal) VALUES ($1, 'approver-a')`, tenant)
}

// openInstruction creates an instruction on a fresh mandate, unevaluated.
func (e *env) openInstruction(record string) (mandate, instruction, digest string) {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction(record, m)
	return m, inst["instruction_id"].(string), inst["action_digest"].(string)
}

func (e *env) approve(who, instruction string, rev int64, digest string, approved bool) (int, map[string]any) {
	e.t.Helper()
	return e.call("POST", "/v1/instructions/"+instruction+"/approvals", who,
		map[string]any{"action_revision": rev, "action_digest": digest, "approved": approved})
}

func requireDeny(t *testing.T, what string, st int, body map[string]any, reason string) {
	t.Helper()
	if st != 200 || body["decision"] != "DENY" || body["reason"] != reason {
		t.Fatalf("%s: status %d %v, want DENY %s", what, st, body, reason)
	}
}

// T17: an approval binds the exact action, supplier and mandate. Approving
// an old digest is refused; an approval given before the action, supplier or
// mandate changed is stale, and a fresh decision is required.
func TestT17ApprovalDriftIsRejected(t *testing.T) {
	e := newR1Env(t)
	e.approvalPolicy()

	m, id, digest := e.openInstruction("po-t17-1")
	st, body := e.evaluate("agent-a", id, 1)
	requireDeny(t, "without approval", st, body, engine.DenyApprovalRequired)
	requireBuckets(t, "nothing reserved", e.buckets(m), buckets{Available: limit})
	st, body = e.approve("approver-a", id, 1, fmt.Sprintf("%064d", 0), true)
	requireStatus(t, "approve another digest", st, body, 409, "ACTION_CHANGED")
	st, body = e.approve("approver-a", id, 1, digest, true)
	requireStatus(t, "approve", st, body, 201, "")
	if body["decision"] != engine.ApprovalApproved || body["approver_id"] != "approver-a" {
		t.Fatalf("approval %v", body)
	}
	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate approved", st, body, 201, "")

	// The action changes after approval: the new revision needs its own.
	_, id2, digest2 := e.openInstruction("po-t17-2")
	st, body = e.approve("approver-a", id2, 1, digest2, true)
	requireStatus(t, "approve rev 1", st, body, 201, "")
	st, body = e.call("POST", "/v1/instructions/"+id2+"/revisions", "issuer-a", map[string]any{"amendment_id": "amd-t17", "expected_action_revision": 1,
		"proposed_action": instructionBody("po-t17-2", "", line("item-a", 3, 19_000_000))["proposed_action"]})
	requireStatus(t, "amend", st, body, 201, "")
	st, body = e.evaluate("agent-a", id2, 2)
	requireDeny(t, "changed action", st, body, engine.DenyApprovalRequired)
	st, body = e.approve("approver-a", id2, 1, digest2, true)
	requireStatus(t, "approve the old revision", st, body, 409, "REVISION_STALE")

	// The mandate changes after approval (a new control epoch).
	m3, id3, digest3 := e.openInstruction("po-t17-3")
	st, body = e.approve("approver-a", id3, 1, digest3, true)
	requireStatus(t, "approve", st, body, 201, "")
	e.adminBypass(`UPDATE steelmatter.mandates SET control_epoch = control_epoch + 1 WHERE mandate_id = $1`, m3)
	st, body = e.evaluate("agent-a", id3, 1)
	requireDeny(t, "mandate changed", st, body, engine.DenyApprovalStale)
	requireBuckets(t, "nothing reserved", e.buckets(m3), buckets{Available: limit})

	// The supplier changes after approval: the old authorisation is stale;
	// after the amendment the new action needs a fresh approval.
	_, id4, digest4 := e.openInstruction("po-t17-4")
	st, body = e.approve("approver-a", id4, 1, digest4, true)
	requireStatus(t, "approve", st, body, 201, "")
	e.admin(`UPDATE target.suppliers SET revision = revision + 1 WHERE tenant_id = $1 AND supplier_id = 'supplier-a'`, tenant)
	st, body = e.evaluate("agent-a", id4, 1)
	requireStatus(t, "supplier changed", st, body, 409, "SUPPLIER_REVISION_STALE")
	st, body = e.call("POST", "/v1/instructions/"+id4+"/revisions", "issuer-a", map[string]any{"amendment_id": "amd-t17-4", "expected_action_revision": 1,
		"proposed_action": instructionBody("po-t17-4", "")["proposed_action"]})
	requireStatus(t, "amend for the new supplier revision", st, body, 201, "")
	st, body = e.evaluate("agent-a", id4, 2)
	requireDeny(t, "after the supplier change", st, body, engine.DenyApprovalRequired)

	// Actions at or below the threshold need no approval.
	m5 := e.mandate(mandateOpts{})
	e.assign(m5, "agent-a")
	small := e.instruction("po-t17-5", m5, line("item-a", 1, approvalAbove))
	st, body = e.evaluate("agent-a", small["instruction_id"].(string), 1)
	requireStatus(t, "below the threshold", st, body, 201, "")
}

// T18: the requestor cannot approve their own action, and an approver whose
// rights were removed no longer counts: no permit until a fresh decision.
func TestT18ApproverAuthority(t *testing.T) {
	e := newR1Env(t)
	e.approvalPolicy()
	m, id, digest := e.openInstruction("po-t18-1")
	for _, who := range []string{"issuer-a", "agent-a", "owner-a"} {
		st, body := e.approve(who, id, 1, digest, true)
		requireStatus(t, who+" approves", st, body, 403, "FORBIDDEN")
	}
	// A principal that is both the issuer and an entitled approver is still
	// refused (separation of duties, checked in the engine as well).
	e.admin(`INSERT INTO steelmatter.approver_entitlements (tenant_id, approver_principal) VALUES ($1, 'issuer-a')`, tenant)
	_, err := e.eng.RecordApproval(context.Background(), auth.Principal{ID: "issuer-a", Tenant: tenant, Kind: auth.Operator},
		engine.ApprovalRequest{InstructionID: id, ActionRevision: 1, ActionDigest: digest, Approve: true})
	requireFault(t, "issuer approves", err, "FORBIDDEN", "separation of duties")
	if e.count(`SELECT count(*) FROM steelmatter.approvals`) != 0 {
		t.Fatal("a refused approval was recorded")
	}

	st, body := e.approve("approver-a", id, 1, digest, true)
	requireStatus(t, "approve", st, body, 201, "")
	e.admin(`UPDATE steelmatter.approver_entitlements SET enabled = false WHERE approver_principal = 'approver-a'`)
	st, body = e.evaluate("agent-a", id, 1)
	requireDeny(t, "approver rights removed", st, body, engine.DenyApprovalStale)
	requireBuckets(t, "no permit", e.buckets(m), buckets{Available: limit})
	st, body = e.approve("approver-a", id, 1, digest, true)
	requireStatus(t, "approve without rights", st, body, 403, "FORBIDDEN")
	e.admin(`UPDATE steelmatter.approver_entitlements SET enabled = true WHERE approver_principal = 'approver-a'`)
	e.admin(`UPDATE steelmatter.principals SET enabled = false WHERE principal_id = 'approver-a'`)
	st, body = e.evaluate("agent-a", id, 1)
	requireDeny(t, "approver disabled", st, body, engine.DenyApprovalStale)
	e.admin(`UPDATE steelmatter.principals SET enabled = true WHERE principal_id = 'approver-a'`)

	// The latest decision counts: a rejection after an approval stops it.
	st, body = e.approve("approver-a", id, 1, digest, false)
	requireStatus(t, "reject", st, body, 201, "")
	st, body = e.evaluate("agent-a", id, 1)
	requireDeny(t, "rejected", st, body, engine.DenyApprovalRequired)
	st, body = e.approve("approver-a", id, 1, digest, true)
	requireStatus(t, "approve again", st, body, 201, "")
	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "fresh approval", st, body, 201, "")

	// Approvals are evidence: no role rewrites or deletes them.
	if _, err := e.h.Admin.Exec(context.Background(), `UPDATE steelmatter.approvals SET decision = 'APPROVED'`); err == nil {
		t.Fatal("an approval was rewritten")
	}
	if _, err := e.h.Admin.Exec(context.Background(), `DELETE FROM steelmatter.approvals`); err == nil {
		t.Fatal("an approval was deleted")
	}
}

// T19: two approved actions complete together with room for only one. The
// approvals reserve nothing; exactly one permit is issued, and the other is
// denied for capacity.
func TestT19ApprovalCapacityRace(t *testing.T) {
	e := newR1Env(t)
	e.approvalPolicy()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	var ids []string
	for _, rec := range []string{"po-t19-1", "po-t19-2"} {
		inst := e.instruction(rec, m)
		id := inst["instruction_id"].(string)
		st, body := e.approve("approver-a", id, 1, inst["action_digest"].(string), true)
		requireStatus(t, "approve "+rec, st, body, 201, "")
		ids = append(ids, id)
	}
	requireBuckets(t, "approvals grant no capacity", e.buckets(m), buckets{Available: limit})
	var wg sync.WaitGroup
	type res struct {
		st   int
		body map[string]any
	}
	out := make([]res, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := e.evaluate("agent-a", id, 1)
			out[i] = res{st, body}
		}()
	}
	wg.Wait()
	issued, denied := 0, 0
	for _, r := range out {
		switch {
		case r.st == 201:
			issued++
		case r.st == 200 && r.body["reason"] == "CAPACITY_EXHAUSTED":
			denied++
		default:
			t.Fatalf("unexpected evaluation %d %v", r.st, r.body)
		}
	}
	if issued != 1 || denied != 1 {
		t.Fatalf("issued %d, denied %d", issued, denied)
	}
	requireBuckets(t, "one reservation", e.buckets(m), buckets{Available: limit - sixLakh, Reserved: sixLakh})
}

// Simulation gives the same answer as evaluation for approvals (T30 parity).
func TestT17SimulationNeedsApprovalToo(t *testing.T) {
	e := newR1Env(t)
	e.approvalPolicy()
	_, id, digest := e.openInstruction("po-t17-sim")
	st, body := e.call("POST", "/v1/actions/simulate", "operator-a", map[string]any{"instruction_id": id, "action_revision": 1})
	requireDeny(t, "simulate without approval", st, body, engine.DenyApprovalRequired)
	st, body = e.approve("approver-a", id, 1, digest, true)
	requireStatus(t, "approve", st, body, 201, "")
	st, body = e.call("POST", "/v1/actions/simulate", "operator-a", map[string]any{"instruction_id": id, "action_revision": 1})
	if st != 200 || body["decision"] != "PERMIT_WOULD_ISSUE" {
		t.Fatalf("simulate approved: %d %v", st, body)
	}
}
