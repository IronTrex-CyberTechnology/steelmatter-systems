package contract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// depthCase is one row of fixtures/vectors/depth-v1.json (CLR-01). The same
// file is run by packages/verifier/test/depth.test.ts so both runtimes agree
// on the nesting-depth boundary.
type depthCase struct {
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	Arrays  int    `json:"arrays"`
	Inner   string `json:"inner"`
	Suffix  string `json:"suffix"`
	Deepest int    `json:"deepest"`
	Expect  string `json:"expect"`
}

func TestParseStrictDepthBoundary(t *testing.T) {
	b, err := os.ReadFile("../../fixtures/vectors/depth-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		MaxDepth int         `json:"max_depth"`
		Cases    []depthCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.MaxDepth != MaxDepth {
		t.Fatalf("fixture max_depth %d; MaxDepth %d", fx.MaxDepth, MaxDepth)
	}
	var accepts, rejects int
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			in := c.Prefix + strings.Repeat("[", c.Arrays) + c.Inner + strings.Repeat("]", c.Arrays) + c.Suffix
			_, perr := ParseStrict([]byte(in))
			switch c.Expect {
			case "accept":
				if perr != nil {
					t.Fatalf("rejected (%v); deepest value at depth %d must be accepted", perr, c.Deepest)
				}
			default:
				requireCode(t, perr, Code(c.Expect))
			}
		})
		if c.Expect == "accept" {
			accepts++
		} else {
			rejects++
		}
	}
	// Both sides of the boundary must be pinned.
	if accepts == 0 || rejects == 0 {
		t.Fatalf("fixture needs accept and reject cases (accept %d, reject %d)", accepts, rejects)
	}
}
