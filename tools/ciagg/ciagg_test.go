package ciagg

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const pkg = "example/pkg"

func phase(t *testing.T, lines ...string) *PhaseResults {
	t.Helper()
	pr, err := ParseGoTestJSON(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func ev(action, test string) string {
	return `{"Action":"` + action + `","Package":"` + pkg + `","Test":"` + test + `"}`
}

func baseInputs(t *testing.T) Inputs {
	return Inputs{
		Config: Config{
			ActiveStage: "S",
			Stages: map[string]Stage{"S": {
				Phases:        []string{"unit"},
				RequiredTests: []TestRef{{Phase: "unit", Package: pkg, Test: "TestA"}},
			}},
			Cases: map[string]CaseMapping{},
		},
		Catalogue: Catalogue{Baseline: "0.6", Cases: []Case{{ID: "T01", Release: "R0"}, {ID: "T44", Release: "R0"}, {ID: "T17", Release: "R1"}}},
		Phases:    map[string]*PhaseResults{"unit": phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("pass", "TestB"))},
	}
}

func requireGate(t *testing.T, rep Report, want, problemSubstring string) {
	t.Helper()
	if rep.Gate != want {
		t.Fatalf("gate = %s; problems %v", rep.Gate, rep.Problems)
	}
	if problemSubstring == "" {
		return
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, problemSubstring) {
			return
		}
	}
	t.Fatalf("no problem mentions %q: %v", problemSubstring, rep.Problems)
}

func TestGatePassesWhenAllRequiredResultsPass(t *testing.T) {
	rep := Evaluate(baseInputs(t))
	requireGate(t, rep, Pass, "")
	if len(rep.Results) != 3 {
		t.Fatalf("report enumerates %d cases; want every catalogue case", len(rep.Results))
	}
	for _, r := range rep.Results {
		if r.Status != NotRun {
			t.Fatalf("%s = %s; unmapped cases must stay NOT_RUN", r.ID, r.Status)
		}
	}
}

func TestGateFailsWhenRequiredPhaseMissing(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = nil
	requireGate(t, Evaluate(in), Fail, `required phase "unit" produced no results`)
}

func TestGateFailsWhenRequiredTestOmitted(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestB"), ev("pass", "TestB"))
	requireGate(t, Evaluate(in), Fail, "required test example/pkg.TestA (unit) is NOT_RUN")
}

func TestGateFailsWhenAnyTestSkipped(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("skip", "TestB"))
	requireGate(t, Evaluate(in), Fail, "was skipped")
}

func TestGateFailsWhenAnyTestFails(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("fail", "TestB"))
	requireGate(t, Evaluate(in), Fail, "example/pkg.TestB failed")
}

// T44: an earlier failure is never overwritten by a later result for the
// same test, and a test that reports more than once fails the gate.
func TestGateFailureIsNotOverwrittenByLaterPass(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("fail", "TestA"), ev("run", "TestA"), ev("pass", "TestA"),
		ev("run", "TestB"), ev("pass", "TestB"))
	rep := Evaluate(in)
	requireGate(t, rep, Fail, "example/pkg.TestA failed")
	requireGate(t, rep, Fail, "reported 2 results")
	if s := rep.RequiredChecks[0].Status; s != Fail {
		t.Fatalf("required check TestA = %s; want FAIL", s)
	}
}

func TestGateFailsWhenPassIsFollowedByFail(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("fail", "TestA"), ev("run", "TestB"), ev("pass", "TestB"))
	requireGate(t, Evaluate(in), Fail, "example/pkg.TestA failed")
}

func TestGateFailsWhenSkipIsFollowedByPass(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("skip", "TestB"), ev("pass", "TestB"))
	requireGate(t, Evaluate(in), Fail, "example/pkg.TestB reported 2 results")
}

func TestGateFailsWhenARequiredTestPassesTwice(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("pass", "TestB"))
	rep := Evaluate(in)
	requireGate(t, rep, Fail, "example/pkg.TestA reported 2 results")
	if s := rep.RequiredChecks[0].Status; s != Fail {
		t.Fatalf("required check TestA = %s; want FAIL", s)
	}
}

func TestGateFailsWhenTestDoesNotFinish(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"))
	requireGate(t, Evaluate(in), Fail, "did not finish")
}

func TestGateFailsOnBuildFailureOrNonJSONOutput(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"),
		`{"Action":"fail","Package":"example/broken"}`, "# example/broken: syntax error")
	rep := Evaluate(in)
	requireGate(t, rep, Fail, "package failed: example/broken")
	requireGate(t, rep, Fail, "non-JSON output")
}

func TestGateFailsWhenPhaseRanNoTests(t *testing.T) {
	in := baseInputs(t)
	in.Config.Stages["S"] = Stage{Phases: []string{"unit"}}
	in.Phases["unit"] = phase(t)
	requireGate(t, Evaluate(in), Fail, "ran no tests")
}

func TestRequiredCaseMustBePass(t *testing.T) {
	in := baseInputs(t)
	st := in.Config.Stages["S"]
	st.RequiredCases = []string{"T01"}
	in.Config.Stages["S"] = st
	requireGate(t, Evaluate(in), Fail, "required case T01 is NOT_RUN")
}

func TestCasePassesOnlyWhenMappingCompleteAndAllTestsPass(t *testing.T) {
	in := baseInputs(t)
	ref := TestRef{Phase: "unit", Package: pkg, Test: "TestA"}

	in.Config.Cases["T01"] = CaseMapping{Complete: false, Tests: []TestRef{ref}}
	rep := Evaluate(in)
	if rep.Results[0].Status != NotRun || rep.Results[0].DeveloperResult != "TESTS_PASSED_UNREVIEWED" {
		t.Fatalf("incomplete mapping gave %+v", rep.Results[0])
	}

	in.Config.Cases["T01"] = CaseMapping{Complete: true, Tests: []TestRef{ref, {Phase: "unit", Package: pkg, Test: "TestMissing"}}}
	if s := Evaluate(in).Results[0].Status; s != NotRun {
		t.Fatalf("complete mapping with a missing test gave %s", s)
	}

	in.Config.Cases["T01"] = CaseMapping{Complete: true, Tests: []TestRef{ref}}
	rep = Evaluate(in)
	if rep.Results[0].Status != Pass || !rep.ProductTestsExecuted {
		t.Fatalf("complete passing mapping gave %+v", rep.Results[0])
	}
}

func TestCaseFailsWhenAnyMappedTestFails(t *testing.T) {
	in := baseInputs(t)
	in.Phases["unit"] = phase(t, ev("run", "TestA"), ev("pass", "TestA"), ev("run", "TestB"), ev("fail", "TestB"))
	in.Config.Cases["T01"] = CaseMapping{Complete: true, Tests: []TestRef{{Phase: "unit", Package: pkg, Test: "TestB"}}}
	if s := Evaluate(in).Results[0].Status; s != Fail {
		t.Fatalf("status %s", s)
	}
}

func TestUnknownCaseMappingIsRejected(t *testing.T) {
	in := baseInputs(t)
	in.Config.Cases["T99"] = CaseMapping{}
	requireGate(t, Evaluate(in), Fail, "T99 is not in the acceptance catalogue")
}

func TestUndefinedActiveStageFails(t *testing.T) {
	in := baseInputs(t)
	in.Config.ActiveStage = "nope"
	requireGate(t, Evaluate(in), Fail, `active stage "nope" is not defined`)
}

func TestCommandForRunsExactlyTheMappedTests(t *testing.T) {
	got := CommandFor("example.com/m", []TestRef{
		{Phase: "integration", Package: "example.com/m/tests/integration", Test: "TestA"},
		{Phase: "integration", Package: "example.com/m/tests/integration", Test: "TestB/sub"},
		{Phase: "unit", Package: "example.com/m/core/x", Test: "TestC"},
		{Phase: "verifier", Package: "packages/verifier", Test: "evidence.test.ts/T26: removing (records)"},
		{Phase: "sdk", Package: "packages/sdk", Test: "client.test.ts/429 honours Retry-After"},
		{Phase: "mcp", Package: "adapters/mcp", Test: "server.test.ts/ping"},
		{Phase: "demo", Package: "demo", Test: "render.test.ts/T43: a changed recording is detected"},
	})
	want := "go test -tags integration,failpoints -count=1 -run '^(TestA|TestB)$' ./tests/integration && " +
		"go test -count=1 -run '^(TestC)$' ./core/x && " +
		"(cd packages/verifier && node --test --test-name-pattern '^(T26: removing \\(records\\))$' test/evidence.test.ts) && " +
		"(cd packages/sdk && node --test --test-name-pattern '^(429 honours Retry-After)$' test/client.test.ts) && " +
		"(cd adapters/mcp && node --test --test-name-pattern '^(ping)$' test/server.test.ts) && " +
		"(cd demo && node --test --test-name-pattern '^(T43: a changed recording is detected)$' test/render.test.ts)"
	if got != want {
		t.Fatalf("command\n got %s\nwant %s", got, want)
	}
}

// T44 against the real configuration: the day-4 stage requires exactly the
// 31 sprint cases, full R0 requires all 35 R0 cases, every catalogue case is
// enumerated, and a run with no results fails the gate.
func TestRealConfigurationEnumeratesRequiredCases(t *testing.T) {
	var cfg Config
	if err := LoadJSON("../../ci/required.json", &cfg); err != nil {
		t.Fatal(err)
	}
	var cat Catalogue
	b, err := os.ReadFile("../../ci/acceptance-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	sprint := []string{"T01", "T02", "T03", "T04", "T05", "T06", "T07", "T08", "T09", "T10", "T11", "T12", "T13", "T14", "T15", "T16",
		"T20", "T21", "T22", "T23", "T24", "T25", "T26", "T27", "T28", "T31", "T32", "T39", "T40", "T41", "T44"}
	if got := cfg.Stages["sprint-day4"].RequiredCases; strings.Join(got, ",") != strings.Join(sprint, ",") {
		t.Fatalf("sprint-day4 cases %v", got)
	}
	var r0 []string
	for _, c := range cat.Cases {
		if c.Release == "R0" {
			r0 = append(r0, c.ID)
		}
	}
	if got := cfg.Stages["full-R0"].RequiredCases; len(got) != 35 || strings.Join(got, ",") != strings.Join(r0, ",") {
		t.Fatalf("full-R0 cases %v; catalogue R0 %v", got, r0)
	}
	if len(cat.Cases) != 44 {
		t.Fatalf("catalogue has %d cases", len(cat.Cases))
	}
	for _, stage := range []string{"sprint-day4", "full-R0"} {
		c := cfg
		c.ActiveStage = stage
		rep := Evaluate(Inputs{Config: c, Catalogue: cat, Phases: map[string]*PhaseResults{}})
		if rep.Gate != Fail || len(rep.Results) != 44 {
			t.Fatalf("%s with no results: gate %s, %d cases enumerated", stage, rep.Gate, len(rep.Results))
		}
		missing := 0
		for _, p := range rep.Problems {
			if strings.HasPrefix(p, "required case ") {
				missing++
			}
		}
		if missing != len(c.Stages[stage].RequiredCases) {
			t.Fatalf("%s: %d required-case problems; want one per required case", stage, missing)
		}
	}
	for id := range cfg.Cases {
		found := false
		for _, c := range cat.Cases {
			found = found || c.ID == id
		}
		if !found {
			t.Fatalf("mapping for unknown case %s", id)
		}
	}
}
