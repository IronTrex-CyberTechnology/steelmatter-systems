package ciagg

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// StageReport is a checkpoint report for one stage (e.g. sprint-day4) in the
// results-template.json shape plus totals.
type StageReport struct {
	Baseline             string `json:"baseline"`
	ReportPhase          string `json:"report_phase"`
	Stage                string `json:"stage"`
	CandidateCommit      string `json:"candidate_commit"`
	Environment          string `json:"environment"`
	GeneratedAt          string `json:"generated_at"`
	ProductTestsExecuted bool   `json:"product_tests_executed"`
	// CIGate is the aggregate gate of the run's active stage, which may differ
	// from this stage; StageResult is computed for this stage's cases only.
	CIGate               string         `json:"ci_gate"`
	CIActiveStage        string         `json:"ci_active_stage"`
	CIEnforcesStageCases bool           `json:"ci_enforces_stage_cases"`
	StageResult          string         `json:"stage_result"`
	Totals               map[string]int `json:"totals"`
	DeveloperTotals      map[string]int `json:"developer_totals"`
	Results              []CaseResult   `json:"results"`
	OutsideStage         []CaseResult   `json:"remaining_r0_outside_stage"`
}

// BuildStageReport selects the stage's required cases from an aggregate report.
func BuildStageReport(rep Report, cfg Config, cat Catalogue, stage, phase string, now time.Time) (*StageReport, error) {
	st, ok := cfg.Stages[stage]
	if !ok {
		return nil, fmt.Errorf("stage %q is not defined", stage)
	}
	byID := map[string]CaseResult{}
	for _, r := range rep.Results {
		byID[r.ID] = r
	}
	out := &StageReport{Baseline: rep.Baseline, ReportPhase: phase, Stage: stage, CandidateCommit: rep.CandidateCommit,
		Environment: rep.Environment, GeneratedAt: now.UTC().Format(time.RFC3339), ProductTestsExecuted: rep.ProductTestsExecuted,
		CIGate: rep.Gate, CIActiveStage: cfg.ActiveStage, CIEnforcesStageCases: cfg.ActiveStage == stage,
		Totals: map[string]int{Pass: 0, Fail: 0, Blocked: 0, NotRun: 0}, DeveloperTotals: map[string]int{}}
	in := map[string]bool{}
	for _, id := range st.RequiredCases {
		r, ok := byID[id]
		if !ok {
			r = CaseResult{ID: id, Status: NotRun, Evidence: "case absent from the aggregate report"}
		}
		in[id] = true
		out.Results = append(out.Results, r)
		out.Totals[r.Status]++
		dev := r.DeveloperResult
		if dev == "" {
			dev = NotRun
		}
		out.DeveloperTotals[dev]++
	}
	for _, c := range cat.Cases {
		if c.Release == "R0" && !in[c.ID] {
			out.OutsideStage = append(out.OutsideStage, byID[c.ID])
		}
	}
	out.StageResult = "MET"
	if out.Totals[Pass] != len(out.Results) {
		out.StageResult = "NOT_MET"
	}
	return out, nil
}

// Markdown renders the report following templates/RELEASE_TEST_REPORT.txt.
// notes supplies the project-specific prose sections.
func (r *StageReport) Markdown(cat Catalogue, notes string) string {
	names := map[string]string{}
	for _, c := range cat.Cases {
		names[c.ID] = c.Name
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Test and checkpoint report\n\n")
	w("| Field | Value |\n|---|---|\n")
	w("| Baseline | %s |\n| Report phase | %s |\n| Stage | %s (%d cases) |\n", r.Baseline, r.ReportPhase, r.Stage, len(r.Results))
	w("| Candidate commit | `%s` |\n| Environment | %s |\n| Generated | %s |\n", r.CandidateCommit, r.Environment, r.GeneratedAt)
	w("| **Stage result** | **%s** (%d of %d cases PASS) |\n", r.StageResult, r.Totals[Pass], len(r.Results))
	enf := "yes"
	if !r.CIEnforcesStageCases {
		enf = fmt.Sprintf("**no**: CI enforces stage `%s`, which requires no acceptance cases", r.CIActiveStage)
	}
	w("| CI gate (active stage `%s`) | %s |\n| CI enforces this stage's cases | %s |\n\n", r.CIActiveStage, r.CIGate, enf)
	w("## Totals\n\n")
	w("Official case status: **PASS %d · FAIL %d · BLOCKED %d · NOT_RUN %d**\n\n", r.Totals[Pass], r.Totals[Fail], r.Totals[Blocked], r.Totals[NotRun])
	keys := make([]string, 0, len(r.DeveloperTotals))
	for k := range r.DeveloperTotals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var dev []string
	for _, k := range keys {
		dev = append(dev, fmt.Sprintf("%s %d", k, r.DeveloperTotals[k]))
	}
	w("Developer-test outcome of the mapped tests: %s\n\n", strings.Join(dev, " · "))
	var failing, unrun []string
	for _, c := range r.Results {
		switch c.DeveloperResult {
		case Fail:
			failing = append(failing, c.ID)
		case "", NotRun:
			unrun = append(unrun, c.ID)
		}
	}
	if len(failing) > 0 {
		w("**Developer tests failing on this candidate:** %s\n\n", strings.Join(failing, ", "))
	}
	if len(unrun) > 0 {
		w("**Cases with no complete developer run:** %s\n\n", strings.Join(unrun, ", "))
	}
	if len(failing) == 0 && len(unrun) == 0 {
		w("All mapped developer tests ran and passed on this candidate.\n\n")
	}
	w("A case is PASS only when its mapping is marked complete after review and QA reproduction and every mapped test passed. `TESTS_PASSED_UNREVIEWED` means the developer tests ran and passed on this candidate but the case has not yet been confirmed.\n\n")
	w("## Applicable cases\n\n| Case | Name | Status | Developer tests | Seed | Reproduce |\n|---|---|---|---|---|---|\n")
	for _, c := range r.Results {
		cmd := c.Command
		if cmd == "" {
			cmd = "—"
		} else {
			cmd = "`" + strings.ReplaceAll(cmd, "|", "\\|") + "`"
		}
		seed := c.Seed
		if seed == "" {
			seed = "—"
		}
		devr := c.DeveloperResult
		if devr == "" {
			devr = NotRun
		}
		w("| %s | %s | %s | %s | %s | %s |\n", c.ID, names[c.ID], c.Status, devr, seed, cmd)
	}
	if len(r.OutsideStage) > 0 {
		w("\n## Remaining R0 cases outside this stage\n\n| Case | Name | Status |\n|---|---|---|\n")
		for _, c := range r.OutsideStage {
			w("| %s | %s | %s |\n", c.ID, names[c.ID], c.Status)
		}
	}
	if strings.TrimSpace(notes) != "" {
		w("\n%s\n", strings.TrimSpace(notes))
	}
	return b.String()
}
