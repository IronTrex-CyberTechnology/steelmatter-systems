package contract

import (
	"encoding/json"
	"testing"
)

func TestR1EvidenceUsesAdmissionTime(t *testing.T) {
	m, p, e, o := chain("2026-10-28T10:00:00Z", "2026-10-28T11:00:00Z",
		"2026-10-28T10:01:00Z", "2026-10-28T10:06:00Z", "2026-10-28T12:00:00Z", "2026-10-28T12:01:00Z")
	e.SchemaVersion, e.AdmittedAt = ExternalEvidenceVersion, "2026-10-28T10:02:00Z"
	if checks := TemporalLinkage(m, p, e, o); len(checks) != 6 || !TemporalConsistent(checks) {
		t.Fatalf("valid admission followed by delayed confirmation: %+v", checks)
	}
	e.AdmittedAt = p.ExpiresAt
	if TemporalConsistent(TemporalLinkage(m, p, e, o)) {
		t.Fatal("admission at permit expiry accepted")
	}
	e.AdmittedAt, e.CommittedAt = "2026-10-28T10:02:00Z", "2026-10-28T10:01:59Z"
	if TemporalConsistent(TemporalLinkage(m, p, e, o)) {
		t.Fatal("settlement before admission accepted")
	}
}

func TestR1EvidenceVersionAndDestinationBinding(t *testing.T) {
	x := packet(t, KindExecution)
	x["schema_version"], x["admitted_at"], x["company_ref"] = "2", x["committed_at"], "acme-in"
	x["target_order_id"] = "PO/2026/001"
	check := func(k Kind, p map[string]any, valid bool) {
		t.Helper()
		raw, _ := json.Marshal(p)
		_, err := Validate(k, raw)
		if (err == nil) != valid {
			t.Fatalf("%s valid=%v: %v", k, valid, err)
		}
	}
	check(KindExecution, x, true)
	x["schema_version"] = "1"
	check(KindExecution, x, false)
	x["schema_version"] = "2"
	delete(x, "admitted_at")
	check(KindExecution, x, false)
	o := packet(t, KindObservation)
	o["schema_version"], o["target_order_id"] = "2", "PO/2026/001"
	o["status"], o["observed_action_digest"] = "MATCH", o["expected_action_digest"]
	o["expected_company_ref"], o["observed_company_ref"], o["observed_state"] = "acme-in", "acme-in", "purchase"
	o["expected_permit_digest"], o["observed_permit_digest"] = x["permit_digest"], x["permit_digest"]
	check(KindObservation, o, true)
	for _, field := range []string{"observed_company_ref", "observed_state", "observed_permit_digest"} {
		old := o[field]
		o[field] = "changed"
		check(KindObservation, o, false)
		o["status"] = "MISMATCH"
		check(KindObservation, o, true)
		o[field], o["status"] = old, "MATCH"
	}
	delete(o, "observed_company_ref")
	check(KindObservation, o, false)
}
