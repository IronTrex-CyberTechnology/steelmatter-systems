//go:build conformance

package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
)

// ADR-003 parity: Go reports exactly the fixture's broken time checks for
// every chain in evidence-temporal-v1.json; packages/verifier/test/temporal.test.ts
// runs the same file through the TypeScript verifier.
func TestConformanceTemporalEvidenceVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "fixtures/vectors/evidence-temporal-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		TrustStore json.RawMessage `json:"trust_store"`
		Cases      []struct {
			Name   string `json:"name"`
			Valid  bool   `json:"valid"`
			Bundle struct {
				Records map[string]string `json:"records"`
			} `json:"bundle"`
			Expect struct {
				Linkage      string   `json:"linkage"`
				Chronology   string   `json:"chronology"`
				BrokenChecks []string `json:"broken_checks"`
			} `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	trust, err := keys.ParseTrustStore(doc.TrustStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 13 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		var m *contract.Mandate
		var p *contract.Permit
		var e *contract.Execution
		var o *contract.Observation
		for _, k := range []contract.Kind{contract.KindMandate, contract.KindPermit, contract.KindExecution, contract.KindObservation} {
			jws, ok := c.Bundle.Records[string(k)+"_jws"]
			if !ok {
				continue
			}
			v, verr := contract.Verify(jws, k, trust)
			if verr != nil {
				t.Fatalf("%s: %s record must be authentic: %v", c.Name, k, verr)
			}
			switch x := v.Value.(type) {
			case *contract.Mandate:
				m = x
			case *contract.Permit:
				p = x
			case *contract.Execution:
				e = x
			case *contract.Observation:
				o = x
			}
		}
		got := []string{}
		missing := false
		for _, r := range contract.TemporalLinkage(m, p, e, o) {
			switch r.Status {
			case contract.TemporalMismatch:
				got = append(got, r.Name)
			case contract.TemporalMissing:
				missing = true
			}
		}
		if !reflect.DeepEqual(got, c.Expect.BrokenChecks) {
			t.Errorf("%s: broken %v, want %v", c.Name, got, c.Expect.BrokenChecks)
		}
		if consistent := len(got) == 0; consistent != (c.Expect.Linkage != "BROKEN") {
			t.Errorf("%s: expected linkage %s but Go temporal consistency is %v", c.Name, c.Expect.Linkage, consistent)
		}
		if missing != (c.Expect.Linkage == "INCOMPLETE") {
			t.Errorf("%s: missing records %v for expected linkage %s", c.Name, missing, c.Expect.Linkage)
		}
		// Same chronology status the TypeScript verifier reports.
		chronology := "CONSISTENT"
		if len(got) > 0 {
			chronology = "INCONSISTENT"
		} else if missing {
			chronology = "INCOMPLETE"
		}
		if chronology != c.Expect.Chronology || c.Valid != (len(got) == 0) {
			t.Errorf("%s: chronology %s (valid %v), fixture %s (valid %v)", c.Name, chronology, len(got) == 0, c.Expect.Chronology, c.Valid)
		}
	}
}
