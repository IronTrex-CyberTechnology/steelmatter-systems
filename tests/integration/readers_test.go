//go:build integration

package integration

import (
	"context"
	"testing"
)

// ADR-002: issuer read access to operation status and evidence follows the
// issuer's current entitlement to the instruction's source workflow, not who
// created the instruction. These two tests pin both transitions.

// readAs returns the status of GET operation and GET evidence for who.
func (e *env) readAs(who, op string) (int, map[string]any, int, map[string]any) {
	e.t.Helper()
	st, body := e.call("GET", "/v1/operations/"+op, who, nil)
	est, ebody := e.call("GET", "/v1/operations/"+op+"/evidence", who, nil)
	return st, body, est, ebody
}

// requireSameAsAbsent checks a refused read is indistinguishable from a read
// of an operation ID that does not exist (no existence oracle, T23).
func requireSameAsAbsent(t *testing.T, e *env, what, who string, body map[string]any, absentPath string) {
	t.Helper()
	ast, absent := e.call("GET", absentPath, who, nil)
	requireStatus(t, what+" (absent control)", ast, absent, 404, "NOT_FOUND")
	if body["error_code"] != absent["error_code"] || body["message"] != absent["message"] {
		t.Fatalf("%s: refused read %v differs from absent-operation read %v", what, body, absent)
	}
}

func TestADR002IssuerLosesReadAccessWhenEntitlementRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.committed() // instruction created by issuer-a, entitled to the source workflow

	st, body, est, ebody := e.readAs("issuer-a", p.operation)
	requireStatus(t, "entitled issuer reads operation", st, body, 200, "")
	requireStatus(t, "entitled issuer reads evidence", est, ebody, 200, "")

	e.admin(`DELETE FROM steelmatter.issuer_entitlements WHERE tenant_id = $1 AND issuer_principal = 'issuer-a' AND source_system = $2`, tenant, source)

	st, body, est, ebody = e.readAs("issuer-a", p.operation)
	requireStatus(t, "issuer after entitlement removed: operation", st, body, 404, "NOT_FOUND")
	requireStatus(t, "issuer after entitlement removed: evidence", est, ebody, 404, "NOT_FOUND")
	if _, leaked := ebody["records"]; leaked {
		t.Fatal("historical evidence records disclosed after entitlement removal")
	}
	requireSameAsAbsent(t, e, "operation after entitlement removed", "issuer-a", body, "/v1/operations/op-does-not-exist")
	requireSameAsAbsent(t, e, "evidence after entitlement removed", "issuer-a", ebody, "/v1/operations/op-does-not-exist/evidence")

	// Other readers are unaffected by the issuer's entitlement change.
	for _, who := range []string{"agent-a", "owner-a", "observer-a", "operator-a"} {
		st, body, est, ebody := e.readAs(who, p.operation)
		requireStatus(t, who+" reads operation", st, body, 200, "")
		requireStatus(t, who+" reads evidence", est, ebody, 200, "")
	}

	// Restoring the entitlement restores access: the rule is evaluated per request.
	e.admin(`INSERT INTO steelmatter.issuer_entitlements (tenant_id, issuer_principal, source_system) VALUES ($1, 'issuer-a', $2)`, tenant, source)
	st, body, est, ebody = e.readAs("issuer-a", p.operation)
	requireStatus(t, "issuer after entitlement restored: operation", st, body, 200, "")
	requireStatus(t, "issuer after entitlement restored: evidence", est, ebody, 200, "")
}

func TestADR002NewlyEntitledIssuerGainsReadAccessWithoutCreatingInstruction(t *testing.T) {
	e := newEnv(t)
	p := e.committed()

	var creator string
	if err := e.h.Admin.QueryRow(context.Background(),
		`SELECT issuer_principal FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`, tenant, p.instruction).Scan(&creator); err != nil {
		t.Fatal(err)
	}
	if creator == "issuer-b" {
		t.Fatal("precondition: issuer-b must not be the creating issuer")
	}

	st, body, est, ebody := e.readAs("issuer-b", p.operation)
	requireStatus(t, "unentitled issuer: operation", st, body, 404, "NOT_FOUND")
	requireStatus(t, "unentitled issuer: evidence", est, ebody, 404, "NOT_FOUND")
	requireSameAsAbsent(t, e, "unentitled issuer", "issuer-b", body, "/v1/operations/op-does-not-exist")

	e.admin(`INSERT INTO steelmatter.issuer_entitlements (tenant_id, issuer_principal, source_system) VALUES ($1, 'issuer-b', $2)`, tenant, source)

	st, body, est, ebody = e.readAs("issuer-b", p.operation)
	requireStatus(t, "newly entitled issuer: operation", st, body, 200, "")
	requireStatus(t, "newly entitled issuer: evidence", est, ebody, 200, "")
	if body["operation_id"] != nil && body["operation_id"] != p.operation {
		t.Fatalf("operation_id %v, want %s", body["operation_id"], p.operation)
	}

	// Entitlement to a different source workflow does not grant access.
	e.admin(`DELETE FROM steelmatter.issuer_entitlements WHERE tenant_id = $1 AND issuer_principal = 'issuer-b'`, tenant)
	e.admin(`INSERT INTO steelmatter.issuer_entitlements (tenant_id, issuer_principal, source_system) VALUES ($1, 'issuer-b', 'synthetic-erp-b')`, tenant)
	st, body, est, ebody = e.readAs("issuer-b", p.operation)
	requireStatus(t, "issuer entitled to another workflow: operation", st, body, 404, "NOT_FOUND")
	requireStatus(t, "issuer entitled to another workflow: evidence", est, ebody, 404, "NOT_FOUND")
}
