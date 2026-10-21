//go:build integration

package integration

import (
	"context"
	"testing"
)

func (e *env) simulate(who, instructionID string, rev int64) (int, map[string]any) {
	e.t.Helper()
	return e.call("POST", "/v1/actions/simulate", who, map[string]any{"instruction_id": instructionID, "action_revision": rev})
}

// snapshot counts everything a simulation must never create or change.
func (e *env) snapshot(mandateID string) [6]int64 {
	e.t.Helper()
	var s [6]int64
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM steelmatter.issuances),
		(SELECT count(*) FROM target.purchase_orders),
		(SELECT count(*) FROM steelmatter.outbox),
		(SELECT available_minor FROM steelmatter.mandates WHERE mandate_id = $1),
		(SELECT reserved_minor FROM steelmatter.mandates WHERE mandate_id = $1),
		(SELECT count(*) FROM steelmatter.issuances WHERE permit_jws IS NOT NULL)`, mandateID).
		Scan(&s[0], &s[1], &s[2], &s[3], &s[4], &s[5]); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// T30: repeated simulations create no reservation, permit or target effect.
func TestT30SimulationIsolation(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	before := e.snapshot(m)
	for i := 0; i < 5; i++ {
		st, body := e.simulate("operator-a", id, 1)
		requireStatus(t, "simulate", st, body, 200, "")
		if body["simulation"] != true || body["decision"] != "PERMIT_WOULD_ISSUE" {
			t.Fatalf("simulation result %v", body)
		}
	}
	if after := e.snapshot(m); after != before {
		t.Fatalf("simulation changed state: before %v after %v", before, after)
	}
	requireLedger(t, e.ledger(m), ledger{Available: limit})

	// The real evaluation still reserves exactly once afterwards.
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate after simulations", st, body, 201, "")
	requireLedger(t, e.ledger(m), reservedLedger)

	// A simulation that would be denied reports the reason and changes nothing.
	small := e.mandate(mandateOpts{limit: 1000})
	e.assign(small, "agent-a")
	id2 := e.instruction("po-request-002", small)["instruction_id"].(string)
	before = e.snapshot(small)
	st, body = e.simulate("operator-a", id2, 1)
	requireStatus(t, "denied simulation", st, body, 200, "CAPACITY_EXHAUSTED")
	if body["simulation"] != true || body["decision"] != "DENY" {
		t.Fatalf("denied simulation %v", body)
	}
	if after := e.snapshot(small); after != before {
		t.Fatalf("denied simulation changed state")
	}
	// Conflicts match evaluate.
	st, body = e.simulate("operator-a", id, 2)
	requireStatus(t, "unknown revision", st, body, 409, "REVISION_STALE")
	st, body = e.simulate("operator-a", "inst-unknown", 1)
	requireStatus(t, "unknown instruction", st, body, 404, "NOT_FOUND")
}

// Simulate is operator-only; everyone else is refused without effect.
func TestSimulatePermissions(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	for _, who := range []string{"agent-a", "issuer-a", "owner-a", "observer-a"} {
		st, body := e.simulate(who, id, 1)
		requireStatus(t, "simulate as "+who, st, body, 403, "FORBIDDEN")
	}
	st, body := e.simulate("", id, 1)
	requireStatus(t, "unauthenticated", st, body, 401, "UNAUTHENTICATED")
	st, body = e.call("POST", "/v1/actions/simulate", "operator-a", map[string]any{"instruction_id": id, "action_revision": 1, "amount_minor": 1})
	requireStatus(t, "closed body", st, body, 400, "UNKNOWN_FIELD")
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances`); n != 0 {
		t.Fatal("refused simulations created state")
	}
}

// Assurance: the five reader kinds of the tenant; unknown adapter 404.
func TestAdapterAssurance(t *testing.T) {
	e := newEnv(t)
	path := "/v1/adapters/" + audience + "/assurance"
	for _, who := range []string{"agent-a", "issuer-a", "owner-a", "observer-a", "operator-a", "agent-x"} {
		st, body := e.call("GET", path, who, nil)
		requireStatus(t, "assurance as "+who, st, body, 200, "")
		if body["adapter_id"] != audience || body["simulation"] != true || len(body) != 7 {
			t.Fatalf("assurance body %v", body)
		}
		ops := body["supported_operations"].([]any)
		if len(ops) != 1 || ops[0] != "purchase_order.issue" {
			t.Fatalf("supported operations %v", ops)
		}
		for _, k := range []string{"supported_paths", "limitations", "test_references"} {
			if len(body[k].([]any)) == 0 {
				t.Fatalf("%s is empty", k)
			}
		}
	}
	st, body := e.call("GET", "/v1/adapters/other-destination/assurance", "operator-a", nil)
	requireStatus(t, "unknown adapter", st, body, 404, "NOT_FOUND")
	st, body = e.call("GET", path, "", nil)
	requireStatus(t, "unauthenticated", st, body, 401, "UNAUTHENTICATED")
}
