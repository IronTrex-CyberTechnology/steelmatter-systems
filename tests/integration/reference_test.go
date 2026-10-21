//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/Steelmatter/examples/procurement"
)

// T43: the pinned demo results (fixtures/demo/reference-results-v1.json) must
// match a fresh run of every scenario: same fixtures, cases, test IDs, final
// ledger and step outcomes. Changing an outcome or a fixture without
// regenerating the reference fails here. Regenerate (then review the diff) with
// STEELMATTER_UPDATE_DEMO_REFERENCE=1.
func TestT43DemoEvidenceConsistency(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	var out bytes.Buffer
	env := e.demoEnv(&out)
	env.FixtureRoot = root
	results := procurement.RunAll(context.Background(), env, "all")
	for _, r := range results {
		requireScenario(t, r, &out)
		if len(r.TestIDs) == 0 {
			t.Fatalf("scenario %s has no test IDs in its trace", r.Scenario)
		}
	}
	path := filepath.Join(root, procurement.ReferencePath)
	if os.Getenv("STEELMATTER_UPDATE_DEMO_REFERENCE") == "1" {
		ref, err := procurement.BuildReference(results, root, commitOf(t, root))
		if err != nil {
			t.Fatal(err)
		}
		if err := ref.Write(path); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s; review the diff before committing", procurement.ReferencePath)
		return
	}
	ref, err := procurement.LoadReference(path)
	if err != nil {
		t.Fatal(err)
	}
	if diffs := ref.Check(results, root); len(diffs) > 0 {
		t.Fatalf("demo results drifted from %s (regenerate with STEELMATTER_UPDATE_DEMO_REFERENCE=1 and review):\n%s",
			procurement.ReferencePath, strings.Join(diffs, "\n"))
	}
}

// commitOf names the source commit, marked -dirty when the tree has changes.
func commitOf(t *testing.T, root string) string {
	t.Helper()
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(head))
	if dirty, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output(); len(dirty) > 0 {
		c += "-dirty"
	}
	return c
}
