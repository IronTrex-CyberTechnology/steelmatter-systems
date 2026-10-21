package contract

import (
	"reflect"
	"testing"
)

func chain(mI, mX, pI, pX, eC, oO string) (*Mandate, *Permit, *Execution, *Observation) {
	return &Mandate{IssuedAt: mI, ExpiresAt: mX}, &Permit{IssuedAt: pI, ExpiresAt: pX},
		&Execution{CommittedAt: eC}, &Observation{ObservedAt: oO}
}

func broken(cs []TemporalCheck) []string {
	out := []string{}
	for _, c := range cs {
		if c.Status == TemporalMismatch {
			out = append(out, c.Name)
		}
	}
	return out
}

// ADR-003: cross-record timestamp rules.
func TestTemporalLinkageRules(t *testing.T) {
	const d = "2026-10-19T10:"
	cases := []struct {
		name               string
		mI, mX, pI, pX, eC string
		oO                 string
		want               []string
	}{
		{"consistent", d + "00:00Z", d + "59:00Z", d + "01:00Z", d + "06:00Z", d + "02:00Z", d + "03:00Z", []string{}},
		{"boundaries are equal", d + "00:00Z", d + "05:00Z", d + "00:00Z", d + "05:00Z", d + "00:00Z", d + "00:00Z", []string{}},
		{"permit before mandate", d + "01:30Z", d + "59:00Z", d + "01:00Z", d + "06:00Z", d + "02:00Z", d + "03:00Z", []string{"time:permit_issued_within_mandate"}},
		{"permit outlives mandate", d + "00:00Z", d + "05:00Z", d + "01:00Z", d + "06:00Z", d + "02:00Z", d + "03:00Z", []string{"time:permit_expiry_within_mandate"}},
		{"commit before permit", d + "00:00Z", d + "59:00Z", d + "01:00Z", d + "06:00Z", d + "00:30Z", d + "03:00Z", []string{"time:execution_within_permit"}},
		{"commit at permit expiry", d + "00:00Z", d + "59:00Z", d + "01:00Z", d + "06:00Z", d + "06:00Z", d + "07:00Z", []string{"time:execution_within_permit"}},
		{"commit after mandate expiry", d + "00:00Z", d + "01:50Z", d + "01:00Z", d + "05:00Z", d + "01:55Z", d + "03:00Z",
			[]string{"time:permit_expiry_within_mandate", "time:execution_within_mandate"}},
		{"observed before commit", d + "00:00Z", d + "59:00Z", d + "01:00Z", d + "06:00Z", d + "02:00Z", d + "01:30Z", []string{"time:observation_after_execution"}},
		{"permit issued at mandate expiry", d + "00:00Z", d + "01:00Z", d + "01:00Z", d + "02:00Z", d + "01:30Z", d + "03:00Z",
			[]string{"time:permit_issued_within_mandate", "time:permit_expiry_within_mandate", "time:execution_within_mandate"}},
	}
	for _, c := range cases {
		m, p, e, o := chain(c.mI, c.mX, c.pI, c.pX, c.eC, c.oO)
		got := TemporalLinkage(m, p, e, o)
		if len(got) != 5 {
			t.Fatalf("%s: %d checks", c.name, len(got))
		}
		if b := broken(got); !reflect.DeepEqual(b, c.want) {
			t.Errorf("%s: broken %v, want %v", c.name, b, c.want)
		}
	}
}

func TestTemporalLinkageMissingRecords(t *testing.T) {
	m, p, _, _ := chain("2026-10-19T10:00:00Z", "2026-10-19T11:00:00Z", "2026-10-19T10:01:00Z", "2026-10-19T10:06:00Z", "", "")
	got := map[string]TemporalStatus{}
	for _, c := range TemporalLinkage(m, p, nil, nil) {
		got[c.Name] = c.Status
	}
	want := map[string]TemporalStatus{
		"time:permit_issued_within_mandate": TemporalOK, "time:permit_expiry_within_mandate": TemporalOK,
		"time:execution_within_permit": TemporalMissing, "time:execution_within_mandate": TemporalMissing, "time:observation_after_execution": TemporalMissing,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if TemporalConsistent(TemporalLinkage(m, p, nil, nil)) != true {
		t.Fatal("missing records alone are not a violation")
	}
	// An unparsable time (only reachable by bypassing validation) is a mismatch, never OK.
	bad, _, _, _ := chain("not-a-time", "2026-10-19T11:00:00Z", "", "", "", "")
	if TemporalConsistent(TemporalLinkage(bad, p, nil, nil)) {
		t.Fatal("unparsable time accepted")
	}
}
