//go:build integration && failpoints

package integration

import (
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// The R1 endpoints over real HTTP (api/openapi.yaml tags dispatches and
// recovery): same engine calls as the D5 tests, now through strict parsing,
// principal-kind checks and the error catalogue.

func (e *env) addPrincipal(id, kind string) {
	e.t.Helper()
	token, hash, err := auth.NewToken()
	if err != nil {
		e.t.Fatal(err)
	}
	e.admin(`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ($1, $2, $3, $4)`, id, tenant, kind, hash)
	e.tokens[id] = token
}

func (e *env) httpOperators() {
	for _, id := range []string{"uday", "kaif", "aakhil"} {
		e.addPrincipal(id, "OPERATOR")
	}
}

func dispatchBody(p permitted) map[string]any {
	return map[string]any{"action": p.action, "permit_jws": p.jws}
}

func TestR1HTTPDispatchSettleAndRead(t *testing.T) {
	e := newEnv(t)
	a := withAnchor(t, e)
	p := e.permitted()
	path := "/v1/dispatches/" + p.operation

	st, body := e.call("POST", "/v1/dispatches", "agent-a", map[string]any{"action": p.action, "permit_jws": p.jws, "destination": "x"})
	requireStatus(t, "unknown field", st, body, 400, "UNKNOWN_FIELD")
	st, body = e.call("POST", "/v1/dispatches", "operator-a", dispatchBody(p))
	requireStatus(t, "operator dispatch", st, body, 403, "FORBIDDEN")

	st, body = e.call("POST", "/v1/dispatches", "agent-a", dispatchBody(p))
	requireStatus(t, "dispatch", st, body, 201, "")
	if body["op_ref"] != p.operation || body["execution"] != engine.DispatchPending {
		t.Fatalf("dispatch body %v", body)
	}
	requireBuckets(t, "after dispatch", e.buckets(p.mandate), r1Unresolved)
	st, body = e.call("POST", "/v1/dispatches", "agent-a", dispatchBody(p))
	requireStatus(t, "identical repeat", st, body, 200, "")
	requireBuckets(t, "repeat moves nothing", e.buckets(p.mandate), r1Unresolved)

	st, body = e.call("GET", path, "agent-a", nil)
	requireStatus(t, "actor reads dispatch", st, body, 403, "FORBIDDEN")
	st, body = e.call("GET", path, "observer-a", nil)
	requireStatus(t, "observer reads dispatch", st, body, 200, "")
	if body["execution"] != engine.DispatchPending {
		t.Fatalf("read %v", body)
	}
	st, body = e.call("GET", "/v1/dispatches/op-unknown", "observer-a", nil)
	requireStatus(t, "unknown op", st, body, 404, "NOT_FOUND")

	st, body = e.call("POST", path+"/settle", "observer-a", nil)
	requireStatus(t, "observer settles", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", path+"/settle", "operator-a", map[string]any{"force": true})
	requireStatus(t, "settle body is closed", st, body, 400, "UNKNOWN_FIELD")
	st, body = e.call("POST", path+"/settle", "operator-a", nil)
	requireStatus(t, "settle", st, body, 200, "")
	if body["execution"] != engine.DispatchCommitted || body["destination_order"] == nil {
		t.Fatalf("settle %v", body)
	}
	requireBuckets(t, "committed", e.buckets(p.mandate), r1Committed)

	st, body = e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "operation status", st, body, 200, "")
	if body["execution"] != engine.DispatchCommitted {
		t.Fatalf("operation status %v", body)
	}
	st, body = e.call("GET", "/v1/recovery/write-state", "observer-a", nil)
	requireStatus(t, "write state", st, body, 200, "")
	if body["enabled"] != true {
		t.Fatalf("write state %v", body)
	}
	st, body = e.call("GET", "/v1/recovery/write-state", "agent-a", nil)
	requireStatus(t, "actor reads write state", st, body, 403, "FORBIDDEN")
	a.verify()
}

// TestD5BothRestoredHeldRuleAndRepair driven entirely over HTTP: restore
// check, reconcile, the HELD rule at the gate, finding, prepare, review,
// confirm, with separation of duties enforced through the endpoints.
func TestR1HTTPRecoveryHeldRuleAndRepair(t *testing.T) {
	e := newEnv(t)
	a := withAnchor(t, e)
	e.httpOperators()
	p := e.permitted()
	path := "/v1/dispatches/" + p.operation
	snap := e.snapshotEngine()
	destBefore := a.s.Backup()
	st, body := e.call("POST", "/v1/dispatches", "agent-a", dispatchBody(p))
	requireStatus(t, "dispatch", st, body, 201, "")
	a.s.DropNextReplies(p.operation, 1)
	a.s.LoseNextCreateDone(p.operation, 1)
	st, body = e.call("POST", path+"/settle", "uday", nil)
	requireStatus(t, "settle", st, body, 200, "")
	if body["execution"] != engine.DispatchUnresolved {
		t.Fatalf("lost reply: %v", body)
	}
	e.restoreEngine(snap)
	a.s.Restore(destBefore)

	st, body = e.call("POST", "/v1/recovery/check", "observer-a", nil)
	requireStatus(t, "observer runs check", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/recovery/check", "uday", nil)
	requireStatus(t, "check", st, body, 200, "")
	if body["writes_disabled"] != true || len(body["signals"].([]any)) == 0 {
		t.Fatalf("restore not detected: %v", body)
	}
	st, body = e.call("GET", "/v1/recovery/write-state", "observer-a", nil)
	requireStatus(t, "write state", st, body, 200, "")
	if body["enabled"] != false {
		t.Fatalf("writes still on: %v", body)
	}

	st, body = e.call("POST", "/v1/recovery/reconcile", "uday", map[string]any{})
	requireStatus(t, "reconcile", st, body, 200, "")
	if len(body["recreated"].([]any)) != 1 || e.dispatchStatus(p.operation) != engine.DispatchHeld {
		t.Fatalf("reconcile %v, status %s", body, e.dispatchStatus(p.operation))
	}
	for _, k := range []string{"from_odoo", "flagged", "refenced", "settled", "problems"} {
		if _, ok := body[k].([]any); !ok {
			t.Fatalf("reconcile %s is not an array: %v", k, body)
		}
	}
	requireBuckets(t, "held", e.buckets(p.mandate), r1Unresolved)

	st, body = e.call("POST", "/v1/recovery/resume", "uday", nil)
	requireStatus(t, "gate without finding", st, body, 200, "")
	if body["passed"] != false || body["resumed"] != false {
		t.Fatalf("gate passed without a HELD finding: %v", body)
	}

	st, body = e.call("POST", path+"/findings", "agent-a", map[string]any{"finding": "x", "evidence_ref": "y"})
	requireStatus(t, "actor records finding", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", path+"/findings", "uday", map[string]any{"finding": ""})
	requireStatus(t, "finding missing evidence", st, body, 400, "SCHEMA_VIOLATION")
	st, body = e.call("POST", path+"/findings", "uday", map[string]any{
		"finding":      "Odoo restore erased an order created after a lost reply; outcome unknown",
		"evidence_ref": "incident-2026-10-28/odoo-backup-and-supplier-email"})
	requireStatus(t, "finding", st, body, 201, "")
	requireBuckets(t, "a finding alone moves nothing", e.buckets(p.mandate), r1Unresolved)

	st, body = e.call("POST", path+"/repairs", "uday", map[string]any{"outcome": "REFUND", "evidence_ref": "e"})
	requireStatus(t, "bad outcome", st, body, 400, "SCHEMA_VIOLATION")
	st, body = e.call("POST", path+"/repairs", "uday", map[string]any{
		"outcome": "COMMIT", "evidence_ref": "supplier-confirmation-PO-erased.pdf", "summary": "supplier confirmed delivery"})
	requireStatus(t, "prepare", st, body, 201, "")
	repairID, _ := body["repair_id"].(string)
	if repairID == "" || body["outcome"] != "COMMIT" {
		t.Fatalf("prepare %v", body)
	}
	rpath := path + "/repairs/" + repairID

	st, body = e.call("POST", rpath+"/review", "uday", map[string]any{"approved": true})
	requireStatus(t, "preparer reviews", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", rpath+"/review", "kaif", map[string]any{"approved": "yes"})
	requireStatus(t, "approved must be boolean", st, body, 400, "SCHEMA_VIOLATION")
	st, body = e.call("POST", rpath+"/review", "kaif", map[string]any{"approved": true, "note": "evidence checked against Odoo logs"})
	requireStatus(t, "review", st, body, 200, "")
	if body["approved"] != true {
		t.Fatalf("review %v", body)
	}

	st, body = e.call("POST", rpath+"/confirm", "aakhil", nil)
	requireStatus(t, "confirm while writes off", st, body, 403, "FORBIDDEN")

	st, body = e.call("POST", "/v1/recovery/resume", "uday", nil)
	requireStatus(t, "gate", st, body, 200, "")
	if body["resumed"] != true {
		t.Fatalf("gate with the HELD rule met: %v", body)
	}
	if e.dispatchStatus(p.operation) != engine.DispatchHeld {
		t.Fatal("resuming writes settled a HELD operation")
	}

	st, body = e.call("POST", rpath+"/confirm", "kaif", nil)
	requireStatus(t, "reviewer confirms", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", rpath+"/confirm", "aakhil", nil)
	requireStatus(t, "confirm", st, body, 200, "")
	if body["execution"] != engine.DispatchCommitted {
		t.Fatalf("confirm %v", body)
	}
	requireBuckets(t, "committed by the confirmed repair", e.buckets(p.mandate), r1Committed)
	st, body = e.call("POST", rpath+"/confirm", "aakhil", nil)
	requireStatus(t, "confirm twice", st, body, 409, "ALREADY_COMMITTED")
	if a.count(anchor.RepairConfirmed) != 1 || a.count(anchor.OperatorFinding) != 1 {
		t.Fatal("anchor repair records")
	}
	a.verify()
}

// Without a configured connector or anchor the R1 endpoints answer 503 and
// change nothing.
func TestR1HTTPUnconfiguredIsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.httpOperators()
	for _, c := range []struct{ method, path, who string }{
		{"POST", "/v1/dispatches/op-1/settle", "uday"},
		{"POST", "/v1/recovery/check", "uday"},
		{"POST", "/v1/recovery/reconcile", "uday"},
		{"POST", "/v1/recovery/resume", "uday"},
		{"GET", "/v1/recovery/write-state", "observer-a"},
		{"POST", "/v1/dispatches/op-1/repairs/rep-1/confirm", "aakhil"},
	} {
		st, body := e.call(c.method, c.path, c.who, nil)
		requireStatus(t, c.method+" "+c.path, st, body, 503, "SERVICE_UNAVAILABLE")
	}
	st, body := e.call("POST", "/v1/dispatches/op-1/findings", "uday", map[string]any{"finding": "f", "evidence_ref": "e"})
	requireStatus(t, "finding without anchor", st, body, 503, "SERVICE_UNAVAILABLE")
	st, body = e.call("POST", "/v1/recovery/check", "", nil)
	requireStatus(t, "no credential", st, body, 401, "UNAUTHENTICATED")
}
