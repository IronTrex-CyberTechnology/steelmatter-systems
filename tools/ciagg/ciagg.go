// Package ciagg is the steelmatter-ci aggregate gate (documents 04 and 12, T44).
//
// It reads `go test -json` output for each phase of the active stage and
// fails when a required phase is missing, any test fails, is skipped or does
// not finish, a required test is absent, or a required acceptance case is not
// PASS. GitHub can treat skipped checks as acceptable; this gate does not.
//
// Acceptance cases are NOT_RUN unless their mapping is marked complete and
// every mapped test passed. Partial component checks never mark a case PASS.
package ciagg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Result statuses (glossary).
const (
	Pass    = "PASS"
	Fail    = "FAIL"
	Blocked = "BLOCKED"
	NotRun  = "NOT_RUN"
)

// TestRef names one Go test in one phase.
type TestRef struct {
	Phase   string `json:"phase"`
	Package string `json:"package"`
	Test    string `json:"test"`
}

func (r TestRef) key() string { return r.Package + "." + r.Test }

// Stage is the required set for one implementation stage.
type Stage struct {
	Description   string    `json:"description"`
	Phases        []string  `json:"phases"`
	RequiredTests []TestRef `json:"required_tests"`
	RequiredCases []string  `json:"required_cases"`
}

// CaseMapping links an acceptance case to the tests that cover all of its
// assertions. Complete must only be set after review confirms full coverage.
type CaseMapping struct {
	Complete bool      `json:"complete"`
	Tests    []TestRef `json:"tests"`
	Seed     string    `json:"seed,omitempty"`
}

// Config is ci/required.json.
type Config struct {
	Schema      string                 `json:"schema"`
	ActiveStage string                 `json:"active_stage"`
	Stages      map[string]Stage       `json:"stages"`
	Cases       map[string]CaseMapping `json:"cases"`
}

// Case is one entry of the acceptance catalogue.
type Case struct {
	ID      string `json:"id"`
	Release string `json:"release"`
	Name    string `json:"name"`
}

// Catalogue is acceptance-cases.json.
type Catalogue struct {
	Baseline string `json:"baseline"`
	Cases    []Case `json:"cases"`
}

// CaseResult follows templates/results-template.json. DeveloperResult is an
// extension: the outcome of the mapped developer tests, reported separately
// so that a partial or unreviewed mapping never reads as a case PASS.
type CaseResult struct {
	ID              string `json:"id"`
	Release         string `json:"release"`
	Status          string `json:"status"`
	Command         string `json:"command"`
	Seed            string `json:"seed"`
	Evidence        string `json:"evidence"`
	DeveloperResult string `json:"developer_result,omitempty"`
}

// CheckResult reports one required stage test.
type CheckResult struct {
	TestRef
	Status string `json:"status"`
}

// Report is written to .out/results.json.
type Report struct {
	Baseline             string        `json:"baseline"`
	Stage                string        `json:"stage"`
	CandidateCommit      string        `json:"candidate_commit"`
	Environment          string        `json:"environment"`
	ProductTestsExecuted bool          `json:"product_tests_executed"`
	Gate                 string        `json:"gate"`
	Problems             []string      `json:"problems"`
	RequiredChecks       []CheckResult `json:"required_checks"`
	Results              []CaseResult  `json:"results"`
}

// Outcome is the terminal state of one test in one phase.
type Outcome struct {
	Action string
	Output []string
}

// PhaseResults holds parsed outcomes keyed by "package.Test".
type PhaseResults struct {
	Tests           map[string]*Outcome
	PackageFailures []string
}

type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// ParseGoTestJSON parses a `go test -json` stream. Non-JSON lines (for
// example build errors written to the same file) are recorded as a failure.
func ParseGoTestJSON(r io.Reader) (*PhaseResults, error) {
	pr := &PhaseResults{Tests: map[string]*Outcome{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			pr.PackageFailures = append(pr.PackageFailures, "non-JSON output: "+truncate(string(line), 200))
			continue
		}
		if ev.Test == "" {
			if ev.Action == "fail" {
				pr.PackageFailures = append(pr.PackageFailures, "package failed: "+ev.Package)
			}
			continue
		}
		k := ev.Package + "." + ev.Test
		o := pr.Tests[k]
		if o == nil {
			o = &Outcome{Action: "run"}
			pr.Tests[k] = o
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			o.Action = ev.Action
		case "output":
			if len(o.Output) < 200 {
				o.Output = append(o.Output, ev.Output)
			}
		}
	}
	return pr, sc.Err()
}

// Inputs are everything the gate evaluates.
type Inputs struct {
	Config      Config
	Catalogue   Catalogue
	Phases      map[string]*PhaseResults // nil entry = phase produced no file
	Commit      string
	Environment string
	// Module is the Go module path, used to print package-relative commands.
	Module string
}

// Evaluate computes the report. Gate is PASS only when Problems is empty.
func Evaluate(in Inputs) Report {
	rep := Report{
		Baseline:        in.Catalogue.Baseline,
		Stage:           in.Config.ActiveStage,
		CandidateCommit: in.Commit,
		Environment:     in.Environment,
		Problems:        []string{},
	}
	problem := func(format string, a ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...)) }

	stage, ok := in.Config.Stages[in.Config.ActiveStage]
	if !ok {
		problem("active stage %q is not defined", in.Config.ActiveStage)
	}
	known := map[string]Case{}
	for _, c := range in.Catalogue.Cases {
		known[c.ID] = c
	}
	if len(known) == 0 {
		problem("acceptance catalogue is empty")
	}
	stagePhases := map[string]bool{}
	for _, p := range stage.Phases {
		stagePhases[p] = true
	}

	// Every required phase must have run and be clean.
	for _, p := range stage.Phases {
		pr := in.Phases[p]
		if pr == nil {
			problem("required phase %q produced no results", p)
			continue
		}
		if len(pr.Tests) == 0 {
			problem("required phase %q ran no tests", p)
		}
		for _, f := range pr.PackageFailures {
			problem("phase %q: %s", p, f)
		}
		for _, k := range sortedKeys(pr.Tests) {
			switch pr.Tests[k].Action {
			case "pass":
			case "skip":
				problem("phase %q: %s was skipped; skipped tests are not accepted in a required phase", p, k)
			case "fail":
				problem("phase %q: %s failed", p, k)
			default:
				problem("phase %q: %s did not finish", p, k)
			}
		}
	}

	status := func(ref TestRef) string {
		pr := in.Phases[ref.Phase]
		if pr == nil {
			return NotRun
		}
		o := pr.Tests[ref.key()]
		switch {
		case o == nil:
			return NotRun
		case o.Action == "pass":
			return Pass
		case o.Action == "fail":
			return Fail
		default:
			return NotRun
		}
	}

	for _, ref := range stage.RequiredTests {
		if !stagePhases[ref.Phase] {
			problem("required test %s uses phase %q outside the stage", ref.key(), ref.Phase)
		}
		s := status(ref)
		rep.RequiredChecks = append(rep.RequiredChecks, CheckResult{TestRef: ref, Status: s})
		if s != Pass {
			problem("required test %s (%s) is %s", ref.key(), ref.Phase, s)
		}
	}

	for id := range in.Config.Cases {
		if _, ok := known[id]; !ok {
			problem("case mapping %s is not in the acceptance catalogue", id)
		}
	}

	byID := map[string]string{}
	for _, c := range in.Catalogue.Cases {
		res := CaseResult{ID: c.ID, Release: c.Release, Status: NotRun}
		m, mapped := in.Config.Cases[c.ID]
		if mapped && len(m.Tests) > 0 {
			res.Command = CommandFor(in.Module, m.Tests)
			res.Seed = m.Seed
			var names []string
			allPass, anyFail := true, false
			for _, ref := range m.Tests {
				s := status(ref)
				names = append(names, ref.Phase+":"+ref.key()+"="+s)
				if s == Fail {
					anyFail = true
				}
				if s != Pass {
					allPass = false
				}
			}
			res.Evidence = strings.Join(names, "; ")
			switch {
			case anyFail:
				res.Status, res.DeveloperResult = Fail, Fail
			case allPass && m.Complete:
				res.Status, res.DeveloperResult = Pass, Pass
			case allPass:
				res.DeveloperResult = "TESTS_PASSED_UNREVIEWED"
				res.Evidence = "mapping not yet marked complete by review/QA: " + res.Evidence
			default:
				res.DeveloperResult = NotRun
			}
		}
		if res.Status == Pass {
			rep.ProductTestsExecuted = true
		}
		byID[c.ID] = res.Status
		rep.Results = append(rep.Results, res)
	}

	for _, id := range stage.RequiredCases {
		s, ok := byID[id]
		switch {
		case !ok:
			problem("required case %s is not in the acceptance catalogue", id)
		case s != Pass:
			problem("required case %s is %s", id, s)
		}
	}

	rep.Gate = Pass
	if len(rep.Problems) > 0 {
		rep.Gate = Fail
	}
	return rep
}

// LoadJSON strictly decodes a JSON file.
func LoadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadPhases reads <dir>/<phase>.json for each phase; missing files map to nil.
func LoadPhases(dir string, phases []string) (map[string]*PhaseResults, error) {
	out := map[string]*PhaseResults{}
	for _, p := range phases {
		f, err := os.Open(filepath.Join(dir, p+".json"))
		if errors.Is(err, os.ErrNotExist) {
			out[p] = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		pr, err := ParseGoTestJSON(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("phase %s: %w", p, err)
		}
		out[p] = pr
	}
	return out, nil
}

func sortedKeys(m map[string]*Outcome) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// nodePhaseDirs maps each node:test phase to its package directory.
var nodePhaseDirs = map[string]string{"verifier": "packages/verifier", "sdk": "packages/sdk", "mcp": "adapters/mcp", "demo": "demo"}

// CommandFor returns copy-paste commands that run exactly the given tests.
func CommandFor(module string, tests []TestRef) string {
	type group struct {
		phase, pkg string
		names      []string
	}
	var groups []*group
	idx := map[string]*group{}
	for _, t := range tests {
		k := t.Phase + "|" + t.Package
		g := idx[k]
		if g == nil {
			g = &group{phase: t.Phase, pkg: t.Package}
			idx[k] = g
			groups = append(groups, g)
		}
		g.names = append(g.names, t.Test)
	}
	var cmds []string
	for _, g := range groups {
		if dir, ok := nodePhaseDirs[g.phase]; ok { // node:test phases, one package each
			files := map[string][]string{}
			var order []string
			for _, n := range g.names {
				file, name, _ := strings.Cut(n, "/")
				if _, ok := files[file]; !ok {
					order = append(order, file)
				}
				files[file] = append(files[file], regexp.QuoteMeta(name))
			}
			for _, f := range order {
				cmds = append(cmds, fmt.Sprintf("(cd %s && node --test --test-name-pattern '^(%s)$' test/%s)", dir, strings.Join(files[f], "|"), f))
			}
			continue
		}
		tags := map[string]string{"unit": "", "integration": "-tags integration,failpoints ", "conformance": "-tags conformance "}[g.phase]
		pkg := "./" + strings.TrimPrefix(strings.TrimPrefix(g.pkg, module), "/")
		var names []string
		for _, n := range g.names {
			names = append(names, regexp.QuoteMeta(strings.SplitN(n, "/", 2)[0]))
		}
		cmds = append(cmds, fmt.Sprintf("go test %s-count=1 -run '^(%s)$' %s", tags, strings.Join(uniqStrings(names), "|"), pkg))
	}
	return strings.Join(cmds, " && ")
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
