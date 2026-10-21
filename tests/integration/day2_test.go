//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// I03 (Day 2 scope): mandate creation and revocation without any admission.
// Only an owner creates or revokes; revocation is idempotent, increments the
// control epoch once, releases the unused reservation exactly once, records
// one mandate.revoked event, and blocks further permits and instructions.
// No order, obligation or operation is ever created.
func TestI03MandateCreateAndRevokeWithoutAdmission(t *testing.T) {
	e := newEnv(t)
	req := map[string]any{"actor_ids": []string{"agent-a"}, "audience": audience, "supplier_ids": []string{"supplier-a"},
		"delivery_ids": []string{"warehouse-a"}, "currency": "INR", "limit_minor": limit,
		"expires_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")}

	// Create: owner only.
	for _, who := range []string{"issuer-a", "agent-a", "observer-a"} {
		st, body := e.call("POST", "/v1/mandates", who, req)
		requireStatus(t, "create mandate as "+who, st, body, 403, "FORBIDDEN")
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.mandates`); n != 0 {
		t.Fatalf("%d mandates created by non-owners", n)
	}
	st, created := e.call("POST", "/v1/mandates", "owner-a", req)
	requireStatus(t, "owner creates mandate", st, created, 201, "")
	if created["status"] != "ACTIVE" {
		t.Fatalf("new mandate status %v", created["status"])
	}
	if jws, _ := created["mandate_jws"].(string); strings.Count(jws, ".") != 2 {
		t.Fatalf("mandate is not signed: %q", jws)
	}
	m := created["mandate"].(map[string]any)["mandate_id"].(string)
	requireLedger(t, e.ledger(m), ledger{Available: limit})

	// One unused reservation to release.
	e.assign(m, "agent-a")
	id := e.instruction("po-revoke-001", m)["instruction_id"].(string)
	st, ev := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate", st, ev, 201, "")
	permitID := ev["permit"].(map[string]any)["permit_id"].(string)
	requireLedger(t, e.ledger(m), reservedLedger)

	// Revoke: non-owners get 403, another tenant's owner gets 404; nothing changes.
	path := "/v1/mandates/" + m + "/revoke"
	for _, who := range []string{"agent-a", "issuer-a"} {
		st, body := e.call("POST", path, who, nil)
		requireStatus(t, "revoke as "+who, st, body, 403, "FORBIDDEN")
	}
	st, body := e.call("POST", path, "owner-x", nil)
	requireStatus(t, "revoke as other tenant owner", st, body, 404, "NOT_FOUND")
	requireLedger(t, e.ledger(m), reservedLedger)

	st, first := e.call("POST", path, "owner-a", nil)
	requireStatus(t, "owner revokes", st, first, 200, "")
	if first["status"] != "REVOKED" || first["control_epoch"] != json.Number("2") {
		t.Fatalf("revoke result %v / epoch %v; want REVOKED / 2", first["status"], first["control_epoch"])
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})
	if s, r := e.issuanceStatus(permitID); s != "RELEASED" || r != "REVOKED" {
		t.Fatalf("issuance after revoke %s/%s; want RELEASED/REVOKED", s, r)
	}

	st, again := e.call("POST", path, "owner-a", nil)
	requireStatus(t, "revoke again", st, again, 200, "")
	if again["control_epoch"] != json.Number("2") {
		t.Fatalf("repeat revoke moved the epoch to %v", again["control_epoch"])
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit}) // released exactly once
	if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'mandate.revoked' AND aggregate_id = $1`, m); n != 1 {
		t.Fatalf("%d mandate.revoked events; want 1", n)
	}

	// Revocation blocks new permits and new instructions.
	st, body = e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate after revoke", st, body, 200, "AUTHORITY_REVOKED")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-revoke-002", m))
	requireStatus(t, "create instruction after revoke", st, body, 403, "AUTHORITY_REVOKED")
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE mandate_id = $1 AND status IN ('UNSIGNED','ACTIVE')`, m); n != 0 {
		t.Fatalf("%d live issuances after revoke", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE mandate_id = $1`, m); n != 1 {
		t.Fatalf("%d instructions; want only the one created before revoke", n)
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit}) // no order, obligation or operation
}

// instState is the persisted identity of a trusted instruction.
type instState struct {
	rev                                       int64
	reqDigest, actor, mandate, source, digest string
	revisions                                 int
}

func (e *env) instructionState(id string) instState {
	e.t.Helper()
	var s instState
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT i.current_revision, i.creation_request_digest, i.actor_id, i.mandate_id,
			i.source_record_id, a.action_digest
		FROM steelmatter.instructions i JOIN steelmatter.action_revisions a
		  ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.current_revision
		WHERE i.instruction_id = $1`, id).Scan(&s.rev, &s.reqDigest, &s.actor, &s.mandate, &s.source, &s.digest); err != nil {
		e.t.Fatal(err)
	}
	s.revisions = e.count(`SELECT count(*) FROM steelmatter.action_revisions WHERE instruction_id = $1`, id)
	return s
}

// T10 (changed duplicate): reusing a trusted source record with a different
// material action returns 409 SOURCE_CONFLICT. The original instruction, its
// action digest, revision history and reservation are unchanged, no second
// instruction exists and no order is created. An identical retry returns the
// original instruction with 200.
func TestT10ChangedDuplicateConflictsWithoutMutation(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	before := e.instructionState(p.instruction)
	if before.rev != 1 || before.revisions != 1 {
		t.Fatalf("unexpected starting state %+v", before)
	}

	changed := func(edit func(a map[string]any)) map[string]any {
		b := instructionBody("po-request-001", p.mandate)
		edit(b["proposed_action"].(map[string]any))
		return b
	}
	cases := []struct {
		name string
		body map[string]any
	}{
		{"changed quantity", instructionBody("po-request-001", p.mandate, line("item-a", 4, 20_000_000))},
		{"changed unit price", instructionBody("po-request-001", p.mandate, line("item-a", 3, 19_000_000))},
		{"added line", instructionBody("po-request-001", p.mandate, line("item-a", 3, 20_000_000), line("item-b", 1, 1))},
		{"changed supplier", changed(func(a map[string]any) { a["supplier_id"] = "supplier-b" })},
		{"changed delivery", changed(func(a map[string]any) { a["delivery_id"] = "warehouse-b" })},
	}
	for _, c := range cases {
		st, body := e.call("POST", "/v1/instructions", "issuer-a", c.body)
		requireStatus(t, c.name, st, body, 409, "SOURCE_CONFLICT")
		if after := e.instructionState(p.instruction); after != before {
			t.Fatalf("%s changed the original instruction\n got %+v\nwant %+v", c.name, after, before)
		}
	}

	st, same := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", p.mandate))
	requireStatus(t, "identical retry", st, same, 200, "")
	if same["instruction_id"] != p.instruction {
		t.Fatalf("identical retry returned %v; want %s", same["instruction_id"], p.instruction)
	}

	if after := e.instructionState(p.instruction); after != before {
		t.Fatalf("original instruction changed\n got %+v\nwant %+v", after, before)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions`); n != 1 {
		t.Fatalf("%d instructions; want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE instruction_id = $1`, p.instruction); n != 1 {
		t.Fatalf("%d issuances; want the original 1", n)
	}
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatalf("original permit %s; want ACTIVE", s)
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger) // 0 orders, one reservation
}
