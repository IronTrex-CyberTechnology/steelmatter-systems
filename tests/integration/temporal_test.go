//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
)

// ADR-003 / F-11: a validly signed observation dated before the execution it
// observes is temporally impossible. The service reports the evidence as
// INVALID and the standalone verifier reports linkage BROKEN on exactly that
// rule, while the untouched chain from the same run stays consistent.
func TestEvidenceTemporalLinkageEnforced(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	if st, body := e.reconcile("observer-a", p.operation); st != 200 || body["observation"] != "MATCH" {
		t.Fatalf("reconcile %d %v", st, body)
	}

	// The honest chain: every time rule holds in the standalone verifier.
	st, bundle := e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	requireStatus(t, "evidence", st, bundle, 200, "")
	if bundle["evidence"] != "AVAILABLE" {
		t.Fatalf("honest evidence %v", bundle["evidence"])
	}
	report := e.verifyWithCLI(bundle)
	times := 0
	for _, c := range report["linkage"].(map[string]any)["checks"].([]any) {
		c := c.(map[string]any)
		if len(c["name"].(string)) > 5 && c["name"].(string)[:5] == "time:" {
			times++
			if c["status"] != "OK" {
				t.Fatalf("honest chain: %v", c)
			}
		}
	}
	if times != 5 || report["linkage"].(map[string]any)["status"] != "COMPLETE" || report["chronology"].(map[string]any)["status"] != "CONSISTENT" {
		t.Fatalf("honest chain: %d time checks, linkage %v, chronology %v", times, report["linkage"], report["chronology"])
	}

	// Re-sign the observation with the observer's own (trusted) key, dated a
	// minute before the commit; everything else unchanged.
	obs := e.latestObservation(p.operation)
	var committed time.Time
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT committed_at FROM steelmatter.operations WHERE operation_id = $1`, p.operation).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	obs.ObservedAt = committed.UTC().Truncate(time.Second).Add(-time.Minute).Format("2006-01-02T15:04:05Z")
	raw, _ := json.Marshal(obs)
	_, signers, err := contract.LoadKeyring(e.keysDir)
	if err != nil {
		t.Fatal(err)
	}
	_, observerSigner, _ := contract.SplitObserver(signers)
	jws, err := contract.Sign(contract.KindObservation, raw, observerSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, verr := contract.Verify(jws, contract.KindObservation, e.eng.Trust); verr != nil {
		t.Fatal("the backdated observation must itself be validly signed for this test")
	}
	e.adminBypass(`UPDATE steelmatter.observations SET observation_jws = $1 WHERE operation_id = $2`, jws, p.operation)

	st, op := e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "status", st, op, 200, "")
	if op["evidence"] != "INVALID" {
		t.Fatalf("service evidence %v for an observation dated before its execution", op["evidence"])
	}
	st, bundle = e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	requireStatus(t, "evidence", st, bundle, 200, "")
	if bundle["evidence"] != "INVALID" {
		t.Fatalf("bundle evidence %v", bundle["evidence"])
	}
	report = e.verifyWithCLI(bundle)
	var mismatched []string
	for _, c := range report["linkage"].(map[string]any)["checks"].([]any) {
		if c := c.(map[string]any); c["status"] == "MISMATCH" {
			mismatched = append(mismatched, c["name"].(string))
		}
	}
	if report["linkage"].(map[string]any)["status"] != "BROKEN" || len(mismatched) != 1 || mismatched[0] != "time:observation_after_execution" {
		t.Fatalf("verifier linkage %v, mismatched %v", report["linkage"].(map[string]any)["status"], mismatched)
	}
	if ch := report["chronology"].(map[string]any); ch["status"] != "INCONSISTENT" {
		t.Fatalf("verifier chronology %v", ch)
	}
	if rec := record(report, "observation"); rec["integrity"] != "VALID" || rec["issuer"] != "TRUSTED" {
		t.Fatalf("the record itself is authentic: %v", rec)
	}
}
