package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// The execution values must be the same everywhere a caller can read them:
// the OpenAPI contract, the engine's R1 dispatch statuses, the SDK's
// ExecutionStatus type and the MCP adapter's outcome wording (draft 7). Only
// COMMITTED is success, and the adapter must explain every other value.
func TestExecutionValuesAgreeAcrossAPISDKAndMCP(t *testing.T) {
	d := load(t)
	var spec []string
	for _, v := range get(t, d, "components", "schemas", "OperationStatus", "properties", "execution", "enum").([]any) {
		spec = append(spec, v.(string))
	}
	sort.Strings(spec)

	sdkSrc, err := os.ReadFile("../packages/sdk/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`export type ExecutionStatus = ([^;]+);`).FindSubmatch(sdkSrc)
	if m == nil {
		t.Fatal("ExecutionStatus not found in the SDK")
	}
	var sdk []string
	for _, q := range regexp.MustCompile(`'([A-Z_]+)'`).FindAllSubmatch(m[1], -1) {
		sdk = append(sdk, string(q[1]))
	}
	sort.Strings(sdk)
	if strings.Join(sdk, ",") != strings.Join(spec, ",") {
		t.Fatalf("SDK ExecutionStatus %v != OpenAPI %v", sdk, spec)
	}

	mcpSrc, err := os.ReadFile("../adapters/mcp/src/tools.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const OUTCOME_NOTES: Record<string, string> = \{(.*?)\n\};`).FindSubmatch(mcpSrc)
	if block == nil {
		t.Fatal("OUTCOME_NOTES not found in the MCP adapter")
	}
	notes := map[string]bool{}
	for _, k := range regexp.MustCompile(`(?m)^\s+([A-Z_]+):`).FindAllSubmatch(block[1], -1) {
		notes[string(k[1])] = true
	}
	for _, v := range spec {
		if v == "COMMITTED" {
			if notes[v] {
				t.Fatal("the MCP adapter must not attach a not-confirmed note to COMMITTED")
			}
			continue
		}
		if !notes[v] {
			t.Errorf("MCP adapter has no outcome note for %s", v)
		}
	}

	in := map[string]bool{}
	for _, v := range spec {
		in[v] = true
	}
	for _, v := range []string{engine.DispatchPending, engine.DispatchUnresolved, engine.DispatchCommitted, engine.DispatchFenced,
		engine.DispatchMismatch, engine.DispatchHeld} {
		if !in[v] {
			t.Errorf("engine dispatch status %s is missing from the OpenAPI execution enum", v)
		}
	}
}
