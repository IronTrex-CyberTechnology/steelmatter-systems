//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/examples/procurement"
)

func (e *env) demoEnv(out *bytes.Buffer) *procurement.Env {
	return &procurement.Env{BaseURL: e.url, Tokens: e.tokens, Admin: e.h.Admin, Tenant: tenant,
		RunID: time.Now().Format("150405.000000000"), Out: out}
}

func requireScenario(t *testing.T, r procurement.Result, out *bytes.Buffer) {
	t.Helper()
	if r.Status != "PASS" {
		t.Fatalf("scenario %s: %s\n%s", r.Scenario, r.Failure, out)
	}
}

// T32: untrusted supplier text instructs the agent to change supplier, inflate
// the order, raise its allowance and create orders. The (simulated, already
// manipulated) agent tries all of it; the deterministic gate rejects every
// unauthorised mutation and only the exact permitted order can commit.
func TestT32AgentInjection(t *testing.T) {
	e := newEnv(t)
	var out bytes.Buffer
	env := e.demoEnv(&out)
	r := procurement.RunAll(context.Background(), env, "agent-injection")[0]
	requireScenario(t, r, &out)
	// Independent checks on persisted state.
	if n := e.count(`SELECT count(*) FROM target.purchase_orders WHERE supplier_id <> 'supplier-a' OR amount_minor <> $1`, sixLakh); n != 0 {
		t.Fatalf("%d injected orders reached the target", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.mandates WHERE limit_minor <> $1`, limit); n != 0 {
		t.Fatal("an allowance was raised")
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions`); n != 1 {
		t.Fatalf("%d instructions exist; the agent created its own", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.amendments`); n != 0 {
		t.Fatal("the agent amended the instruction")
	}
	for _, s := range r.Steps {
		if !s.OK {
			t.Fatalf("step %s failed", s.Name)
		}
	}
	raw, err := os.ReadFile("../../fixtures/demo/supplier-message.txt")
	if err != nil || !strings.Contains(string(raw), "ignore your previous instructions") {
		t.Fatal("injection fixture missing")
	}
}

// The CLI demo scenarios pass against the real API in-process.
func TestDemoScenarios(t *testing.T) {
	e := newEnv(t)
	var out bytes.Buffer
	results := procurement.RunAll(context.Background(), e.demoEnv(&out), "all")
	if len(results) != len(procurement.Scenarios) {
		t.Fatalf("ran %d scenarios", len(results))
	}
	for _, r := range results {
		requireScenario(t, r, &out)
	}
}

// T31: a fresh checkout follows docs/quickstart.md (scripts/demo) with
// generated synthetic keys, a fresh local database and no production service,
// credential or model API. With a clean working tree (CI) the checkout is a
// real `git clone` of HEAD; with local uncommitted changes it falls back to a
// copy of tracked and unignored files and says so.
func TestT31CleanInstall(t *testing.T) {
	adminURL := os.Getenv("STEELMATTER_TEST_ADMIN_URL")
	if adminURL == "" {
		t.Skip("STEELMATTER_TEST_ADMIN_URL is not set")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "steelmatter")
	dirty, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	cloned := len(dirty) == 0
	if cloned {
		if out, err := exec.Command("git", "clone", "--quiet", "--no-local", "--no-hardlinks", root, dst).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		t.Log("fresh checkout: git clone of HEAD")
	} else {
		copyWorkingTree(t, root, dst)
		t.Log("working tree has uncommitted changes: used a copy of tracked and unignored files instead of git clone")
	}
	for _, unwanted := range []string{".local", ".out", "dist", "packages/verifier/node_modules"} {
		if _, err := os.Stat(filepath.Join(dst, unwanted)); err == nil {
			t.Fatalf("fresh checkout contains %s", unwanted)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.Contains(k, "API_KEY") || strings.HasPrefix(k, "ANTHROPIC") || strings.HasPrefix(k, "OPENAI") || strings.HasPrefix(k, "STEELMATTER_") {
			continue // no model API credentials or inherited Steelmatter configuration
		}
		env = append(env, kv)
	}
	env = append(env, "STEELMATTER_ADMIN_URL="+adminURL)
	start := time.Now()
	cmd := exec.Command("bash", "scripts/demo") // the documented quickstart command
	cmd.Dir, cmd.Env = dst, env
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("scripts/demo failed in a fresh checkout: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dst, ".out", "demo.json"))
	if err != nil {
		t.Fatalf("demo results missing: %v\n%s", err, out)
	}
	var res struct {
		Simulation bool   `json:"simulation"`
		Commit     string `json:"commit"`
		Reference  struct {
			Checked    bool     `json:"checked"`
			Consistent bool     `json:"consistent"`
			Mismatches []string `json:"mismatches"`
		} `json:"reference"`
		Results []procurement.Result `json:"results"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.Results {
		got[r.Scenario] = r.Status
		for _, s := range r.Steps {
			if !s.OK {
				t.Fatalf("%s step %q failed: %s", r.Scenario, s.Name, s.Detail)
			}
		}
	}
	if !res.Simulation || len(got) != len(procurement.Scenarios) {
		t.Fatalf("demo results %v (simulation=%v)", got, res.Simulation)
	}
	// T43 trace: commit, test IDs and fixture digests, checked against the pinned reference.
	// A copied working tree has no .git, so its commit is "unknown"; a clone must name HEAD.
	if res.Commit == "" || (cloned && res.Commit == "unknown") || !res.Reference.Checked || !res.Reference.Consistent {
		t.Fatalf("demo trace: commit %q, reference %+v", res.Commit, res.Reference)
	}
	for _, r := range res.Results {
		if len(r.TestIDs) == 0 || r.Fixtures == nil {
			t.Fatalf("scenario %s has no test/fixture trace", r.Scenario)
		}
	}
	for _, s := range procurement.Scenarios {
		if got[s.Name] != "PASS" {
			t.Fatalf("scenario %s: %s\n%s", s.Name, got[s.Name], out)
		}
	}
	t.Logf("fresh-checkout demo completed in %s (measured; Go modules from the local module cache)", elapsed.Round(time.Second))
}

func copyWorkingTree(t *testing.T, root, dst string) {
	t.Helper()
	files, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Split(strings.TrimRight(string(files), "\x00"), "\x00") {
		src := filepath.Join(root, f)
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, f), b, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}
