//go:build integration

package integration

import (
	"fmt"
	"sync"
	"testing"
)

// evaluateConcurrently releases every request at once (start barrier) and
// returns status and decision/code per request.
func (e *env) evaluateConcurrently(who []string, instructions []string) []string {
	e.t.Helper()
	out := make([]string, len(instructions))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range instructions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			st, body := e.call("POST", "/v1/actions/evaluate", who[i], map[string]any{"instruction_id": instructions[i], "action_revision": 1})
			switch {
			case st == 201:
				out[i] = "PERMIT_ISSUED"
			case st == 200 && body["decision"] == "DENY":
				out[i] = "DENY:" + body["reason"].(string)
			default:
				out[i] = fmt.Sprintf("%d:%v", st, body["error_code"])
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return out
}

func tally(results []string) map[string]int {
	m := map[string]int{}
	for _, r := range results {
		m[r]++
	}
	return m
}

// T01: two actors concurrently reserve 60,000,000 each from 100,000,000.
func TestT01ConcurrentAllowance(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	a := e.instruction("po-a", m)
	// Second source workflow for agent-b on the same mandate.
	e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ('asg-b', $1, 'synthetic-erp-b', $2, 'agent-b', true)`, tenant, m)
	bb := instructionBody("po-b", m)
	bb["source_system"] = "synthetic-erp-b"
	st, b := e.call("POST", "/v1/instructions", "issuer-a", bb)
	requireStatus(t, "create b", st, b, 201, "")
	if b["actor_id"] != "agent-b" {
		t.Fatal(b["actor_id"])
	}

	got := tally(e.evaluateConcurrently([]string{"agent-a", "agent-b"}, []string{a["instruction_id"].(string), b["instruction_id"].(string)}))
	if got["PERMIT_ISSUED"] != 1 || got["DENY:CAPACITY_EXHAUSTED"] != 1 {
		t.Fatalf("outcomes %v; want exactly one permit and one capacity denial", got)
	}
	requireLedger(t, e.ledger(m), reservedLedger) // available 40,000,000
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE status IN ('UNSIGNED','ACTIVE')`); n != 1 {
		t.Fatalf("%d live reservations; want 1", n)
	}
}

// T02 (fault-free run): 100 concurrent distinct instructions of 10,000,000
// each from 100,000,000 → exactly ten reservations, ninety capacity denials.
func TestT02LoadContentionFaultFree(t *testing.T) {
	e := newEnv(t)
	m, ids, who := e.loadScenario(100)
	got := tally(e.evaluateConcurrently(who, ids))
	if got["PERMIT_ISSUED"] != 10 || got["DENY:CAPACITY_EXHAUSTED"] != 90 {
		t.Fatalf("outcomes %v; want 10 permits and 90 capacity denials", got)
	}
	requireLedger(t, e.ledger(m), ledger{Available: 0, Reserved: limit})
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE status = 'ACTIVE'`); n != 10 {
		t.Fatalf("%d persisted active reservations; want 10", n)
	}
}

// loadScenario creates n instructions of 10,000,000 for agent-a.
func (e *env) loadScenario(n int) (string, []string, []string) {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	ids := make([]string, n)
	who := make([]string, n)
	for i := range ids {
		inst := e.instruction(fmt.Sprintf("load-%03d", i), m, line("item-a", 1, 10_000_000))
		ids[i], who[i] = inst["instruction_id"].(string), "agent-a"
	}
	return m, ids, who
}
