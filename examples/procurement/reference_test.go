package procurement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."

func committedReference(t *testing.T) *Reference {
	t.Helper()
	ref, err := LoadReference(filepath.Join(repoRoot, ReferencePath))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// resultsFrom rebuilds run results that match the reference exactly.
func resultsFrom(ref *Reference) []Result {
	var out []Result
	for _, s := range ref.Scenarios {
		r := Result{Scenario: s.Scenario, Case: s.Case, Status: s.Status, Ledger: s.Ledger, TestIDs: s.TestIDs, Fixtures: map[string]string{}}
		for _, f := range s.Fixtures {
			r.Fixtures[f] = ref.Fixtures[f]
		}
		for _, st := range s.Steps {
			r.Steps = append(r.Steps, Step{Name: st.Name, OK: st.OK, Detail: "run-specific"})
		}
		out = append(out, r)
	}
	return out
}

// T43: the committed reference is labelled, covers every scenario with its
// test IDs, and its fixture digests match the files in this checkout. Editing a
// fixture without regenerating the reference fails here (no database needed).
func TestT43ReferenceMatchesCommittedFixtures(t *testing.T) {
	ref := committedReference(t)
	if diffs := ref.CheckFixtures(repoRoot); len(diffs) > 0 {
		t.Fatalf("fixtures changed without regenerating %s:\n%s", ReferencePath, strings.Join(diffs, "\n"))
	}
	pinned := map[string]ReferenceScenario{}
	for _, s := range ref.Scenarios {
		pinned[s.Scenario] = s
	}
	if len(pinned) != len(Scenarios) {
		t.Fatalf("reference has %d scenarios, demo has %d", len(pinned), len(Scenarios))
	}
	for _, s := range Scenarios {
		p, ok := pinned[s.Name]
		if !ok || p.Case != s.Case || strings.Join(p.TestIDs, ",") != strings.Join(s.TestIDs, ",") || len(p.Fixtures) != len(s.Fixtures) {
			t.Fatalf("scenario %s: reference %+v does not match its definition", s.Name, p)
		}
		for _, f := range s.Fixtures {
			if _, ok := ref.Fixtures[f]; !ok {
				t.Fatalf("scenario %s fixture %s has no pinned digest", s.Name, f)
			}
		}
	}
}

func TestT43CheckDetectsFixtureDrift(t *testing.T) {
	ref := committedReference(t)
	root := t.TempDir()
	for f := range ref.Fixtures {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if diffs := ref.Check(resultsFrom(ref), root); len(diffs) != 0 {
		t.Fatalf("identical copy reported drift: %v", diffs)
	}
	for f := range ref.Fixtures {
		if err := os.WriteFile(filepath.Join(root, f), []byte("changed fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		diffs := ref.Check(resultsFrom(ref), root)
		if len(diffs) == 0 || !strings.Contains(diffs[0], "fixture "+f+" changed") {
			t.Fatalf("changed fixture %s not detected: %v", f, diffs)
		}
		if err := os.Remove(filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
		if diffs := ref.CheckFixtures(root); len(diffs) == 0 {
			t.Fatalf("missing fixture %s not detected", f)
		}
	}
}

func TestT43CheckDetectsOutcomeDrift(t *testing.T) {
	ref := committedReference(t)
	mutations := map[string]func(rs []Result) []Result{
		"status":       func(rs []Result) []Result { rs[0].Status = "FAIL"; return rs },
		"ledger":       func(rs []Result) []Result { rs[0].Ledger = "available INR 0"; return rs },
		"step outcome": func(rs []Result) []Result { rs[0].Steps[0].OK = false; return rs },
		"step renamed": func(rs []Result) []Result { rs[0].Steps[0].Name += " (edited)"; return rs },
		"step added": func(rs []Result) []Result {
			rs[0].Steps = append(rs[0].Steps, Step{Name: "extra", OK: true})
			return rs
		},
		"case":     func(rs []Result) []Result { rs[0].Case = "T99"; return rs },
		"test ids": func(rs []Result) []Result { rs[0].TestIDs = nil; return rs },
		"missing":  func(rs []Result) []Result { return rs[1:] },
		"unknown":  func(rs []Result) []Result { return append(rs, Result{Scenario: "new-scenario", Status: "PASS"}) },
		"fixture used": func(rs []Result) []Result {
			for i := range rs {
				for f := range rs[i].Fixtures {
					rs[i].Fixtures[f] = strings.Repeat("0", 64)
				}
			}
			return rs
		},
	}
	for name, mutate := range mutations {
		if diffs := ref.Check(mutate(resultsFrom(ref)), repoRoot); len(diffs) == 0 {
			t.Errorf("%s drift not detected", name)
		}
	}
}

// T43: the displayed trace must identify the exact code. A reference pinned
// from a dirty working tree, an unknown commit or a short SHA is refused both
// when it is built and when it is loaded.
func TestT43ReferenceRecordsACleanCommit(t *testing.T) {
	if err := checkCommit(committedReference(t).Commit); err != nil {
		t.Fatal(err)
	}
	pass := []Result{{Scenario: "flagship", Status: "PASS"}}
	const sha = "5e204694279a9deac3f69aa37a4a63258f588ad8"
	if _, err := BuildReference(pass, repoRoot, sha); err != nil {
		t.Fatalf("a clean commit must be accepted: %v", err)
	}
	for _, bad := range []string{sha + "-dirty", "unknown", "", "5e20469", strings.ToUpper(sha)} {
		if _, err := BuildReference(pass, repoRoot, bad); err == nil {
			t.Fatalf("BuildReference accepted commit %q", bad)
		}
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, ReferencePath))
	if err != nil {
		t.Fatal(err)
	}
	ref := committedReference(t)
	dirty := strings.Replace(string(b), `"`+ref.Commit+`"`, `"`+sha+`-dirty"`, 1)
	path := filepath.Join(t.TempDir(), "reference.json")
	if err := os.WriteFile(path, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReference(path); err == nil || !strings.Contains(err.Error(), "clean checkout") {
		t.Fatalf("LoadReference accepted a dirty commit: %v", err)
	}
}

func TestBuildReferenceRefusesFailedRun(t *testing.T) {
	if _, err := BuildReference([]Result{{Scenario: "flagship", Status: "FAIL"}}, repoRoot, "5e204694279a9deac3f69aa37a4a63258f588ad8"); err == nil {
		t.Fatal("a failing run was pinned")
	}
}
