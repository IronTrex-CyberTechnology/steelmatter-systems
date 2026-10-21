//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpSession is an MCP client speaking newline-delimited JSON-RPC to the
// adapter process (adapters/mcp) over stdio.
type mcpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *bytes.Buffer
	nextID int
}

func (e *env) startMCP(actor string) *mcpSession {
	e.t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		e.t.Fatal("node is required for the MCP adapter test")
	}
	tokenFile := filepath.Join(e.t.TempDir(), "actor.token")
	if err := os.WriteFile(tokenFile, []byte(e.tokens[actor]+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join("..", "..", "adapters", "mcp", "src", "main.ts"))
	cmd.Env = append(os.Environ(), "STEELMATTER_SERVER="+e.url, "STEELMATTER_MCP_TOKEN_FILE="+tokenFile, "STEELMATTER_MCP_TOKEN=")
	s := &mcpSession{t: e.t, cmd: cmd, stderr: &bytes.Buffer{}}
	cmd.Stderr = s.stderr
	if s.in, err = cmd.StdinPipe(); err != nil {
		e.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	s.out = bufio.NewScanner(stdout)
	s.out.Buffer(make([]byte, 1<<20), 16<<20)
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return s
}

func (s *mcpSession) send(msg map[string]any) {
	s.t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write to adapter: %v\n%s", err, s.stderr)
	}
}

// request sends a request and returns its response (the adapter answers
// sequential requests in order).
func (s *mcpSession) request(method string, params any) map[string]any {
	s.t.Helper()
	s.nextID++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params})
	done := make(chan bool, 1)
	go func() { done <- s.out.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			s.t.Fatalf("adapter closed stdout: %v\n%s", s.out.Err(), s.stderr)
		}
	case <-time.After(30 * time.Second):
		s.t.Fatalf("no MCP response to %s\n%s", method, s.stderr)
	}
	var resp map[string]any
	dec := json.NewDecoder(bytes.NewReader(s.out.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		s.t.Fatalf("stdout line is not JSON-RPC: %v: %s", err, s.out.Bytes())
	}
	if id, _ := resp["id"].(json.Number); id.String() != strings.TrimSpace(jsonString(s.nextID)) {
		s.t.Fatalf("response id %v, want %d", resp["id"], s.nextID)
	}
	return resp
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

// tool calls a tool and returns (isError, structuredContent).
func (s *mcpSession) tool(name string, args map[string]any) (bool, map[string]any) {
	s.t.Helper()
	resp := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: no result: %v", name, resp)
	}
	return res["isError"] == true, res["structuredContent"].(map[string]any)
}

// An agent drives the procurement flow only through the MCP adapter, holding
// only its actor token: it sees just the actor tools, gets a permit, cannot
// widen or change the action (T32 through MCP), commits exactly once, reads
// status and evidence, and is denied beyond the allowance. Persisted state is
// checked directly.
func TestMCPAgentEndToEnd(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-mcp-001", m)["instruction_id"].(string)
	second := e.instruction("po-mcp-002", m)["instruction_id"].(string)

	s := e.startMCP("agent-a")
	init := s.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "go-test", "version": "1"}})
	if init["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %v", init)
	}
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	var names []string
	for _, tl := range s.request("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "steelmatter_evaluate_action,steelmatter_admit_operation,steelmatter_get_operation,steelmatter_get_evidence,steelmatter_get_adapter_assurance" {
		t.Fatalf("tools: %v", names)
	}

	isErr, ev := s.tool("steelmatter_evaluate_action", map[string]any{"instruction_id": inst, "action_revision": 1})
	permit := ev["result"].(map[string]any)
	if isErr || permit["decision"] != "PERMIT_ISSUED" || ev["created"] != true {
		t.Fatalf("evaluate: %v", ev)
	}
	action := permit["action"].(map[string]any)
	jws := permit["permit_jws"].(string)

	// Injected instructions: switch supplier, inflate the order, call tools it does not have.
	switched := clone(action)
	switched["supplier_id"] = "supplier-b"
	if isErr, r := s.tool("steelmatter_admit_operation", map[string]any{"action": switched, "permit_jws": jws}); !isErr || r["error_code"] != "ACTION_CHANGED" {
		t.Fatalf("switched supplier: %v", r)
	}
	inflated := clone(action)
	inflated["lines"] = []any{line("item-a", 15, 20_000_000)}
	inflated["amount_minor"] = 300_000_000
	if isErr, r := s.tool("steelmatter_admit_operation", map[string]any{"action": inflated, "permit_jws": jws}); !isErr || r["error_code"] != "ACTION_CHANGED" {
		t.Fatalf("inflated order: %v", r)
	}
	// A tool the actor does not have is a protocol error.
	if resp := s.request("tools/call", map[string]any{"name": "steelmatter_create_mandate", "arguments": map[string]any{}}); func() bool {
		e, ok := resp["error"].(map[string]any)
		return !ok || e["code"] != json.Number("-32602")
	}() {
		t.Fatalf("unknown tool: %v", resp)
	}
	// Undeclared arguments are tool errors (isError, MCP 2025-11-25) and never reach the service.
	for _, bad := range []map[string]any{
		{"instruction_id": inst, "action_revision": 1, "actor_id": "agent-b"},
		{"action": action, "permit_jws": jws, "destination": "http://elsewhere"},
	} {
		name := "steelmatter_evaluate_action"
		if _, ok := bad["permit_jws"]; ok {
			name = "steelmatter_admit_operation"
		}
		if isErr, r := s.tool(name, bad); !isErr || r["error_kind"] != "INVALID_ARGUMENTS" {
			t.Fatalf("%s %v: %v", name, bad, r)
		}
	}
	requireLedger(t, e.ledger(m), ledger{Available: 40_000_000, Reserved: sixLakh})

	isErr, op := s.tool("steelmatter_admit_operation", map[string]any{"action": action, "permit_jws": jws})
	result := op["result"].(map[string]any)
	if isErr || op["created"] != true || result["execution"] != "COMMITTED" {
		t.Fatalf("admit: %v", op)
	}
	operationID := result["operation_id"].(string)
	if isErr, again := s.tool("steelmatter_admit_operation", map[string]any{"action": action, "permit_jws": jws}); isErr || again["created"] != false ||
		again["result"].(map[string]any)["operation_id"] != operationID {
		t.Fatalf("repeat admit: %v", again)
	}
	if _, st := s.tool("steelmatter_get_operation", map[string]any{"operation_id": operationID}); st["result"].(map[string]any)["execution"] != "COMMITTED" {
		t.Fatalf("operation: %v", st)
	}
	if _, evd := s.tool("steelmatter_get_evidence", map[string]any{"operation_id": operationID}); evd["result"].(map[string]any)["links"].(map[string]any)["obligation_id"] != result["obligation_id"] {
		t.Fatalf("evidence: %v", evd)
	}
	if isErr, a := s.tool("steelmatter_get_adapter_assurance", map[string]any{"adapter_id": "synthetic-procurement"}); isErr || a["result"].(map[string]any)["simulation"] != true {
		t.Fatalf("assurance: %v", a)
	}
	if isErr, d := s.tool("steelmatter_evaluate_action", map[string]any{"instruction_id": second, "action_revision": 1}); isErr || d["result"].(map[string]any)["reason"] != "CAPACITY_EXHAUSTED" {
		t.Fatalf("over allowance: %v", d)
	}

	if err := s.in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.cmd.Wait(); err != nil {
		t.Fatalf("adapter exit: %v\n%s", err, s.stderr)
	}
	if strings.Contains(s.stderr.String(), e.tokens["agent-a"]) {
		t.Fatal("the actor token appeared in adapter diagnostics")
	}
	requireLedger(t, e.ledger(m), ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1, Obligations: 1, Operations: 1, Outbox: 1})
	if n := e.count(`SELECT count(*) FROM steelmatter.mandates WHERE limit_minor <> $1`, limit); n != 0 {
		t.Fatal("an allowance changed")
	}
	if n := e.count(`SELECT count(*) FROM target.purchase_orders WHERE supplier_id <> 'supplier-a' OR amount_minor <> $1`, sixLakh); n != 0 {
		t.Fatal("a changed order reached the target")
	}
}
