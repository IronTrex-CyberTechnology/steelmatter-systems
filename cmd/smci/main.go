// Command smci is the steelmatter-ci aggregate gate and result summariser.
//
//	smci aggregate -config ci/required.json -catalogue ci/acceptance-cases.json -results .out -report .out/results.json
//	smci summary .out/unit.json
//	smci phases -config ci/required.json
//	smci report -stage sprint-day4 -results .out/results.json -json .out/sprint-day4-results.json -md .out/sprint-day4-report.md
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/tools/ciagg"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smci aggregate|summary|phases ...")
		os.Exit(2)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "aggregate":
		code, err = aggregate(os.Args[2:])
	case "summary":
		code, err = summary(os.Args[2:])
	case "phases":
		err = phases(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
		code = 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "smci:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func aggregate(args []string) (int, error) {
	fs := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	cfgPath := fs.String("config", "ci/required.json", "required-results configuration")
	catPath := fs.String("catalogue", "ci/acceptance-cases.json", "acceptance case catalogue")
	resultsDir := fs.String("results", ".out", "directory with <phase>.json files")
	reportPath := fs.String("report", ".out/results.json", "report output path")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	var in ciagg.Inputs
	if err := ciagg.LoadJSON(*cfgPath, &in.Config); err != nil {
		return 1, err
	}
	if err := loadCatalogue(*catPath, &in.Catalogue); err != nil {
		return 1, err
	}
	var phaseNames []string
	if st, ok := in.Config.Stages[in.Config.ActiveStage]; ok {
		phaseNames = st.Phases
	}
	for _, m := range in.Config.Cases {
		for _, t := range m.Tests {
			phaseNames = append(phaseNames, t.Phase)
		}
	}
	var err error
	if in.Phases, err = ciagg.LoadPhases(*resultsDir, uniq(phaseNames)); err != nil {
		return 1, err
	}
	in.Commit = commit()
	in.Module = module()
	in.Environment = fmt.Sprintf("%s %s/%s ci=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH, os.Getenv("CI"))

	rep := ciagg.Evaluate(in)
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return 1, err
	}
	if err := os.WriteFile(*reportPath, append(b, '\n'), 0o644); err != nil {
		return 1, err
	}

	counts := map[string]int{}
	for _, r := range rep.Results {
		counts[r.Status]++
	}
	fmt.Printf("steelmatter-ci stage %s at %s\n", rep.Stage, rep.CandidateCommit)
	for _, c := range rep.RequiredChecks {
		fmt.Printf("  required %-7s %s:%s.%s\n", c.Status, c.Phase, c.Package, c.Test)
	}
	fmt.Printf("  acceptance cases: PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d (of %d)\n",
		counts[ciagg.Pass], counts[ciagg.Fail], counts[ciagg.Blocked], counts[ciagg.NotRun], len(rep.Results))
	for _, p := range rep.Problems {
		fmt.Println("  PROBLEM:", p)
	}
	fmt.Printf("gate: %s (report %s)\n", rep.Gate, *reportPath)
	if rep.Gate != ciagg.Pass {
		return 1, nil
	}
	return 0, nil
}

// loadCatalogue tolerates extra catalogue fields (given_when, assertion, status).
func loadCatalogue(path string, c *ciagg.Catalogue) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, c)
}

func summary(args []string) (int, error) {
	if len(args) != 1 {
		return 2, fmt.Errorf("usage: smci summary FILE")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return 1, err
	}
	defer f.Close()
	pr, err := ciagg.ParseGoTestJSON(f)
	if err != nil {
		return 1, err
	}
	counts := map[string]int{}
	var bad []string
	for k, o := range pr.Tests {
		counts[o.Action]++
		if o.Action != "pass" {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	for _, k := range bad {
		o := pr.Tests[k]
		fmt.Printf("--- %s %s\n%s", strings.ToUpper(o.Action), k, strings.Join(o.Output, ""))
	}
	for _, f := range pr.PackageFailures {
		fmt.Println("---", f)
	}
	fmt.Printf("%s: pass=%d fail=%d skip=%d unfinished=%d package_failures=%d\n",
		args[0], counts["pass"], counts["fail"], counts["skip"], counts["run"], len(pr.PackageFailures))
	if len(bad) > 0 || len(pr.PackageFailures) > 0 || len(pr.Tests) == 0 {
		return 1, nil
	}
	return 0, nil
}

func phases(args []string) error {
	fs := flag.NewFlagSet("phases", flag.ContinueOnError)
	cfgPath := fs.String("config", "ci/required.json", "required-results configuration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var cfg ciagg.Config
	if err := ciagg.LoadJSON(*cfgPath, &cfg); err != nil {
		return err
	}
	st, ok := cfg.Stages[cfg.ActiveStage]
	if !ok {
		return fmt.Errorf("active stage %q is not defined", cfg.ActiveStage)
	}
	fmt.Println(strings.Join(st.Phases, " "))
	return nil
}

func commit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "status", "--porcelain").Output(); err != nil || len(st) > 0 {
		c += "-dirty"
	}
	return c
}

func uniq(in []string) []string {
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

func module() string {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "module"))
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	cfgPath := fs.String("config", "ci/required.json", "required-results configuration")
	catPath := fs.String("catalogue", "ci/acceptance-cases.json", "acceptance case catalogue")
	resultsPath := fs.String("results", ".out/results.json", "aggregate report from smci aggregate")
	stage := fs.String("stage", "sprint-day4", "stage whose required cases to report")
	phase := fs.String("phase", "INTEGRATED_DAY_4", "report phase label")
	notesPath := fs.String("notes", "ci/sprint-report-notes.md", "project notes appended to the Markdown report")
	jsonOut := fs.String("json", ".out/sprint-day4-results.json", "JSON output")
	mdOut := fs.String("md", ".out/sprint-day4-report.md", "Markdown output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var cfg ciagg.Config
	if err := ciagg.LoadJSON(*cfgPath, &cfg); err != nil {
		return err
	}
	var cat ciagg.Catalogue
	if err := loadCatalogue(*catPath, &cat); err != nil {
		return err
	}
	var rep ciagg.Report
	b, err := os.ReadFile(*resultsPath)
	if err != nil {
		return fmt.Errorf("%w (run scripts/ci first)", err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return err
	}
	sr, err := ciagg.BuildStageReport(rep, cfg, cat, *stage, *phase, time.Now())
	if err != nil {
		return err
	}
	jb, _ := json.MarshalIndent(sr, "", "  ")
	if err := os.WriteFile(*jsonOut, append(jb, '\n'), 0o644); err != nil {
		return err
	}
	notes, _ := os.ReadFile(*notesPath)
	if err := os.WriteFile(*mdOut, []byte(sr.Markdown(cat, string(notes))), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s report: PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d (developer tests: %v)\nwrote %s and %s\n",
		*stage, sr.Totals["PASS"], sr.Totals["FAIL"], sr.Totals["BLOCKED"], sr.Totals["NOT_RUN"], sr.DeveloperTotals, *jsonOut, *mdOut)
	return nil
}
