package contract

import (
	"encoding/json"
	"os"
	"testing"
)

// TestActionTaxVectors runs fixtures/vectors/action-tax-v1.json (D1 v1.4, R1
// decisions 1 and 2). packages/verifier/test/tax.test.ts runs the same file.
func TestActionTaxVectors(t *testing.T) {
	b, err := os.ReadFile("../../fixtures/vectors/action-tax-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct {
			Name    string          `json:"name"`
			Expect  string          `json:"expect"`
			Payload json.RawMessage `json:"payload"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, verr := Validate(KindAction, c.Payload)
			if c.Expect == "accept" {
				if verr != nil {
					t.Fatalf("rejected: %v", verr)
				}
				return
			}
			requireCode(t, verr, Code(c.Expect))
		})
	}
}

// A v1 action without the D1 v1.4 fields keeps its canonical form and
// digest: the new fields are omitted, never written as null or zero.
func TestActionWithoutTaxKeepsDigest(t *testing.T) {
	raw, err := os.ReadFile("../../fixtures/packet/action.payload.json")
	if err != nil {
		t.Fatal(err)
	}
	v, verr := Validate(KindAction, raw)
	if verr != nil {
		t.Fatal(verr)
	}
	again, _ := json.Marshal(v)
	c1, d1, err := CanonicalDigest(KindAction, raw)
	if err != nil {
		t.Fatal(err)
	}
	c2, d2, err := CanonicalDigest(KindAction, again)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || string(c1) != string(c2) {
		t.Fatalf("round trip changed the action:\n%s\n%s", c1, c2)
	}
}

func TestNormaliseLinesSortsTaxes(t *testing.T) {
	out, err := NormaliseLines([]Line{{ItemID: "a", Quantity: 1, UnitPriceMinor: 1, TaxIDs: []string{"gst-18", "cess-1"}}})
	if err != nil || out[0].TaxIDs[0] != "cess-1" {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := NormaliseLines([]Line{{ItemID: "a", Quantity: 1, UnitPriceMinor: 1, TaxIDs: []string{"gst-18", "gst-18"}}}); err == nil {
		t.Fatal("repeated tax accepted")
	}
}
