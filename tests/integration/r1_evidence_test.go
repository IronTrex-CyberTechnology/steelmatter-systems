//go:build integration && failpoints

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// assertR1Evidence exercises both signed records, actual observer readback,
// API export and the independent TypeScript verifier. Also used by real Odoo.
func assertR1Evidence(t *testing.T, e *env, p permitted) map[string]any {
	t.Helper()
	ctx := context.Background()
	if _, err := e.eng.SignPendingExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ObserveUnobserved(ctx); err != nil {
		t.Fatal(err)
	}
	st, b := e.call("GET", "/v1/operations/"+p.operation+"/evidence", "observer-a", nil)
	requireStatus(t, "R1 evidence", st, b, 200, "")
	if b["evidence"] != "AVAILABLE" || len(b["incomplete"].([]any)) != 0 {
		t.Fatalf("incomplete R1 evidence: %v", b)
	}
	r := e.verifyWithCLI(b)
	if r["linkage"].(map[string]any)["status"] != "COMPLETE" || r["chronology"].(map[string]any)["status"] != "CONSISTENT" || r["observation"].(map[string]any)["status"] != "MATCH" {
		t.Fatalf("R1 evidence failed standalone verification: %v", r)
	}
	for _, kind := range []string{"execution", "observation"} {
		jws := b["records"].(map[string]any)[kind+"_jws"].(string)
		v, err := contract.Verify(jws, contract.Kind(kind), e.eng.Trust)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(v.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["schema_version"] != "2" {
			t.Fatalf("%s is not R1 evidence", kind)
		}
	}
	return b
}

func TestR1EvidenceAfterPermitExpiryAndRestore(t *testing.T) {
	e := newEnv(t)
	a := withAnchor(t, e)
	e.eng.Cfg.PermitTTL = 10 * time.Second
	p := e.permitted()
	snap := e.snapshotEngine()
	mustDispatch(t, e, p)
	v, err := contract.Verify(p.jws, contract.KindPermit, e.eng.Trust)
	if err != nil {
		t.Fatal(err)
	}
	expiry, _ := contract.ParseTimestamp(v.Value.(*contract.Permit).ExpiresAt)
	if wait := time.Until(expiry.Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	requireExecution(t, "confirmation after permit expiry", e.settle(p.operation, a.s), engine.DispatchCommitted)
	assertR1Evidence(t, e, p)
	e.restoreEngine(snap)
	a.check()
	if rep := a.reconcile(); len(rep.Problems) != 0 {
		t.Fatalf("recovery: %+v", rep)
	}
	assertR1Evidence(t, e, p)
	requireBuckets(t, "evidence after restore", e.buckets(p.mandate), r1Committed)
	if a.s.Creates() != 1 || e.count(`SELECT count(*) FROM steelmatter.r1_executions`) != 1 || e.count(`SELECT count(*) FROM steelmatter.obligations`) != 1 {
		t.Fatal("duplicate order or evidence")
	}
}

type alteredReadback struct {
	connector.Connector
	alter func(*connector.Result)
}

func (c alteredReadback) Observe(ctx context.Context, a connector.Action) (connector.Result, error) {
	r, err := c.Connector.Observe(ctx, a)
	if err == nil {
		c.alter(&r)
	}
	return r, err
}

func TestR1ObserverDetectsDestinationChangesWithoutMovingMoney(t *testing.T) {
	e := newEnv(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	e.settle(p.operation, a.s)
	assertR1Evidence(t, e, p)
	for _, tc := range []struct {
		name, want string
		alter      func(*connector.Result)
	}{
		{"company", "MISMATCH", func(r *connector.Result) { r.Destination.CompanyRef = "another-company" }},
		{"state", "MISMATCH", func(r *connector.Result) { r.Destination.State = "cancel" }},
		{"permit", "MISMATCH", func(r *connector.Result) { r.Destination.PermitDigest = "changed" }},
		{"supplier revision", "MISMATCH", func(r *connector.Result) { r.Destination.SupplierRevision++ }},
		{"amount", "MISMATCH", func(r *connector.Result) { r.Destination.AmountMinor++ }},
		{"missing order", "INSUFFICIENT_EVIDENCE", func(r *connector.Result) { r.Outcome, r.Destination = connector.NotFound, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.eng.Connector = alteredReadback{a.s, tc.alter}
			st, b := e.reconcile("observer-a", p.operation)
			requireStatus(t, tc.name, st, b, 200, "")
			if b["observation"] != tc.want {
				t.Fatalf("%s: %v", tc.name, b)
			}
			_, b = e.call("GET", "/v1/operations/"+p.operation+"/evidence", "observer-a", nil)
			r := e.verifyWithCLI(b)
			if r["linkage"].(map[string]any)["status"] != "COMPLETE" || r["observation"].(map[string]any)["status"] != tc.want {
				t.Fatalf("signed readback: %v", r)
			}
			requireBuckets(t, "readback cannot refund", e.buckets(p.mandate), r1Committed)
		})
	}
	e.eng.Connector = a.s
	if a.s.Creates() != 1 {
		t.Fatal("observer recreated order")
	}
	for _, sql := range []string{`UPDATE steelmatter.dispatches SET amount_minor=1`, `UPDATE steelmatter.r1_executions SET execution_jws='x'`, `SELECT * FROM steelmatter.dispatches`, `SELECT * FROM steelmatter.evidence_observations`} {
		if _, err := e.h.Observer.Exec(context.Background(), sql); err == nil {
			t.Fatalf("observer allowed: %s", sql)
		}
	}
	st, b := e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-x", nil)
	requireStatus(t, "other tenant evidence", st, b, 404, "NOT_FOUND")
}
