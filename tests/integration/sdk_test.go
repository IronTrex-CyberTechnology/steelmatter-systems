//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/api"
)

// runSDK runs the TypeScript SDK's live test (packages/sdk/test/live) against
// this environment's real HTTP service and PostgreSQL.
func (e *env) runSDK(scenario, mandateID string) {
	e.t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		e.t.Fatal("node is required for the SDK integration test")
	}
	toks, _ := json.Marshal(e.tokens)
	cmd := exec.Command(node, "--test", "--test-reporter=spec", "test/live/integration.live.ts")
	cmd.Dir = filepath.Join("..", "..", "packages", "sdk")
	cmd.Env = append(os.Environ(),
		"SM_SDK_BASE_URL="+e.url, "SM_SDK_TOKENS="+string(toks), "SM_SDK_MANDATE="+mandateID,
		"SM_SDK_TRUST="+filepath.Join(e.keysDir, "trust-store.json"), "SM_SDK_SCENARIO="+scenario)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("SDK live test (%s) failed: %v\n%s", scenario, err, out)
	}
	e.t.Logf("SDK live test (%s):\n%s", scenario, out)
}

// The TypeScript SDK drives all 11 operations end to end: status codes,
// created/existing results, DENY, error envelopes, tamper and actor checks,
// and evidence that verifies offline. Persisted state is asserted here too.
func TestSDKEndToEnd(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	e.runSDK("flow", m)

	requireLedger(t, e.ledger(m), ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1, Obligations: 1, Operations: 1, Outbox: 1})
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE mandate_id = $1`, m); n != 2 {
		t.Fatalf("%d instructions; want 2 (the SDK's repeat and conflict created none)", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.amendments`); n != 1 {
		t.Fatalf("%d amendments; the exact retry must not add one", n)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.mandates WHERE status = 'REVOKED'`); n != 1 {
		t.Fatalf("%d revoked mandates", n)
	}
}

// The SDK honours 429 Retry-After from the service's per-principal quota.
func TestSDKRateLimitRetry(t *testing.T) {
	e := newEnv(t, envOpts{limiter: api.NewLimiter(600, 1)})
	e.runSDK("quota", "")
}
