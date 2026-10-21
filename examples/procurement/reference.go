package procurement

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ReferencePath is the committed demo reference (T43), relative to the
// repository root. Displayed demo results are generated from a pinned run and
// compared with it; any drift in outcomes or fixtures fails the check until
// the reference is regenerated and reviewed.
const ReferencePath = "fixtures/demo/reference-results-v1.json"

const referenceSchema = "steelmatter-demo-reference/1"

// Reference is the pinned outcome of every demo scenario.
type Reference struct {
	Schema      string `json:"schema"`
	Simulation  bool   `json:"simulation"`
	Synthetic   bool   `json:"synthetic"`
	Note        string `json:"note"`
	Commit      string `json:"generated_at_commit"`
	GeneratedBy string `json:"generated_by"`
	// Fixtures maps each fixture path to its SHA-256.
	Fixtures  map[string]string   `json:"fixtures"`
	Scenarios []ReferenceScenario `json:"scenarios"`
}

// ReferenceScenario is the expected, run-independent outcome of one scenario.
type ReferenceScenario struct {
	Scenario string          `json:"scenario"`
	Case     string          `json:"case"`
	TestIDs  []string        `json:"test_ids"`
	Fixtures []string        `json:"fixtures"`
	Status   string          `json:"status"`
	Ledger   string          `json:"ledger"`
	Steps    []ReferenceStep `json:"steps"`
}

// ReferenceStep is a step name and whether it held. Step details carry
// run-specific IDs and are not compared.
type ReferenceStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

func fixtureDigest(root, path string) string {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return "unavailable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func reduce(r Result) ReferenceScenario {
	rs := ReferenceScenario{Scenario: r.Scenario, Case: r.Case, TestIDs: r.TestIDs, Fixtures: []string{}, Status: r.Status, Ledger: r.Ledger}
	for f := range r.Fixtures {
		rs.Fixtures = append(rs.Fixtures, f)
	}
	sort.Strings(rs.Fixtures)
	for _, s := range r.Steps {
		rs.Steps = append(rs.Steps, ReferenceStep{Name: s.Name, OK: s.OK})
	}
	return rs
}

// BuildReference pins results. Every scenario must have passed.
func BuildReference(results []Result, root, commit string) (*Reference, error) {
	ref := &Reference{Schema: referenceSchema, Simulation: true, Synthetic: true,
		Note:        "Simulation with synthetic data. Pinned demo outcomes; regenerate only through review.",
		Commit:      commit,
		GeneratedBy: "STEELMATTER_UPDATE_DEMO_REFERENCE=1 go test -tags integration,failpoints -run '^TestT43DemoEvidenceConsistency$' ./tests/integration",
		Fixtures:    map[string]string{}}
	for _, r := range results {
		if r.Status != "PASS" {
			return nil, fmt.Errorf("scenario %s did not pass; refusing to pin it", r.Scenario)
		}
		ref.Scenarios = append(ref.Scenarios, reduce(r))
		for f := range r.Fixtures {
			ref.Fixtures[f] = fixtureDigest(root, f)
		}
	}
	return ref, nil
}

// LoadReference reads a reference file, rejecting unknown fields.
func LoadReference(path string) (*Reference, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var ref Reference
	if err := dec.Decode(&ref); err != nil {
		return nil, fmt.Errorf("demo reference %s: %w", path, err)
	}
	if ref.Schema != referenceSchema || !ref.Simulation || !ref.Synthetic || ref.Commit == "" {
		return nil, fmt.Errorf("demo reference %s: wrong schema or missing simulation/synthetic/commit labels", path)
	}
	return &ref, nil
}

// CheckFixtures reports fixtures whose current content differs from the reference.
func (ref *Reference) CheckFixtures(root string) []string {
	var out []string
	paths := make([]string, 0, len(ref.Fixtures))
	for p := range ref.Fixtures {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if got := fixtureDigest(root, p); got != ref.Fixtures[p] {
			out = append(out, fmt.Sprintf("fixture %s changed (sha256 %s, reference %s)", p, got, ref.Fixtures[p]))
		}
	}
	return out
}

// Check compares a full demo run with the reference: fixtures, scenario set,
// cases, test IDs, status, final ledger and step outcomes.
func (ref *Reference) Check(results []Result, root string) []string {
	out := ref.CheckFixtures(root)
	want := map[string]ReferenceScenario{}
	for _, s := range ref.Scenarios {
		want[s.Scenario] = s
	}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.Scenario] = true
		w, ok := want[r.Scenario]
		if !ok {
			out = append(out, fmt.Sprintf("scenario %s is not in the reference", r.Scenario))
			continue
		}
		got := reduce(r)
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(w)
		if !bytes.Equal(gj, wj) {
			out = append(out, fmt.Sprintf("scenario %s differs from the reference:\n  got  %s\n  want %s", r.Scenario, gj, wj))
		}
		for f, d := range r.Fixtures {
			if ref.Fixtures[f] != d {
				out = append(out, fmt.Sprintf("scenario %s ran with fixture %s sha256 %s; reference %s", r.Scenario, f, d, ref.Fixtures[f]))
			}
		}
	}
	for _, s := range ref.Scenarios {
		if !seen[s.Scenario] {
			out = append(out, fmt.Sprintf("reference scenario %s did not run", s.Scenario))
		}
	}
	return out
}

// Write stores the reference with stable formatting.
func (ref *Reference) Write(path string) error {
	b, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
