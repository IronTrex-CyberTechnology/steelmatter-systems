package procurement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Step is one checked step of a scenario.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Result is the outcome of one scenario. Status is PASS only if every
// expectation held.
type Result struct {
	Scenario string `json:"scenario"`
	Title    string `json:"title"`
	Case     string `json:"case"`
	Status   string `json:"status"`
	Steps    []Step `json:"steps"`
	Ledger   string `json:"ledger,omitempty"`
	Failure  string `json:"failure,omitempty"`
	// Trace (T43): the tests that cover this scenario and the SHA-256 of each
	// fixture it uses, so displayed results identify exactly what produced them.
	TestIDs  []string          `json:"test_ids"`
	Fixtures map[string]string `json:"fixtures"`
}

// Scenario is a named demo.
type Scenario struct {
	Name  string
	Title string
	Case  string
	Run   func(ctx context.Context, e *Env, r *Recorder) error
	// TestIDs are the Go tests that exercise this scenario or its case.
	TestIDs []string
	// Fixtures are repository-relative fixture files the scenario depends on.
	Fixtures []string
}

// Scenarios lists the demo scenarios in presentation order.
var Scenarios = []Scenario{
	{"flagship", "Shared allowance: two INR 6,00,000 requests against INR 10,00,000", "T01/T09", flagship,
		[]string{"TestDemoScenarios", "TestT01ConcurrentAllowance", "TestT09Replay"}, nil},
	{"stale-supplier", "Changed supplier record invalidates an issued permit", "T04", staleSupplier,
		[]string{"TestDemoScenarios", "TestT04StaleSupplier"}, nil},
	{"lost-response", "Lost HTTP response after commit", "T13", lostResponse,
		[]string{"TestDemoScenarios", "TestT13LostResponse"}, nil},
	{"agent-injection", "Injected supplier text cannot widen what the agent may do", "T32", agentInjection,
		[]string{"TestDemoScenarios", "TestT32AgentInjection"}, []string{"fixtures/demo/supplier-message.txt"}},
}

// Recorder collects and prints steps.
type Recorder struct {
	out io.Writer
	res *Result
}

func (r *Recorder) note(format string, a ...any) {
	fmt.Fprintf(r.out, "      %s\n", fmt.Sprintf(format, a...))
}

// expect records a step; a false condition fails the scenario.
func (r *Recorder) expect(name string, ok bool, format string, a ...any) error {
	detail := fmt.Sprintf(format, a...)
	r.res.Steps = append(r.res.Steps, Step{Name: name, OK: ok, Detail: detail})
	mark := "[ok]  "
	if !ok {
		mark = "[FAIL]"
	}
	fmt.Fprintf(r.out, "  %s %s — %s\n", mark, name, detail)
	if !ok {
		return fmt.Errorf("%s: %s", name, detail)
	}
	return nil
}

// RunAll runs the named scenarios ("all" for every one).
func RunAll(ctx context.Context, e *Env, names ...string) []Result {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []Result
	for _, s := range Scenarios {
		if !want["all"] && !want[s.Name] {
			continue
		}
		out = append(out, RunOne(ctx, e, s))
	}
	return out
}

// RunOne runs a single scenario and prints its narrative.
func RunOne(ctx context.Context, e *Env, s Scenario) Result {
	res := Result{Scenario: s.Name, Title: s.Title, Case: s.Case, TestIDs: s.TestIDs, Fixtures: map[string]string{}}
	fmt.Fprintf(e.Out, "\n== %s [%s]\n   %s\n   Simulation · synthetic data · no real ERP, bank or model\n", s.Name, s.Case, s.Title)
	fmt.Fprintf(e.Out, "   tests: %s\n", strings.Join(s.TestIDs, ", "))
	for _, f := range s.Fixtures {
		d := fixtureDigest(e.FixtureRoot, f)
		res.Fixtures[f] = d
		fmt.Fprintf(e.Out, "   fixture: %s sha256:%s\n", f, d)
	}
	r := &Recorder{out: e.Out, res: &res}
	if err := s.Run(ctx, e, r); err != nil {
		res.Status, res.Failure = "FAIL", err.Error()
	} else {
		res.Status = "PASS"
	}
	fmt.Fprintf(e.Out, "   result: %s\n", res.Status)
	return res
}

// ---- building blocks ----

func line(item string, qty, price int64) map[string]any {
	return map[string]any{"item_id": item, "quantity": qty, "unit_price_minor": price}
}

func proposal(supplier string, lines ...map[string]any) map[string]any {
	return map[string]any{"supplier_id": supplier, "delivery_id": "warehouse-a", "currency": "INR", "lines": lines}
}

func (e *Env) mandate(ctx context.Context, limit int64, actors, suppliers []string) (string, error) {
	r := e.do(ctx, "POST", "/v1/mandates", "owner-a", map[string]any{
		"actor_ids": actors, "audience": "synthetic-procurement", "supplier_ids": suppliers, "delivery_ids": []string{"warehouse-a"},
		"currency": "INR", "limit_minor": limit, "expires_at": time.Now().UTC().Add(2 * time.Hour).Format("2006-01-02T15:04:05Z"),
	})
	if r.Err != nil || r.Status != 201 {
		return "", fmt.Errorf("create mandate: %d %s %v", r.Status, r.code(), r.Err)
	}
	return r.Body["mandate"].(map[string]any)["mandate_id"].(string), nil
}

// assign is trusted operator configuration: which actor executes
// instructions from a source workflow under a mandate.
func (e *Env) assign(ctx context.Context, mandateID, source, actor string) error {
	_, err := e.Admin.Exec(ctx, `INSERT INTO steelmatter.workflow_assignments (assignment_id, tenant_id, source_system, mandate_id, actor_id, enabled)
		VALUES ($1, $2, $3, $4, $5, true)`, "asg-"+mandateID+"-"+source, e.Tenant, source, mandateID, actor)
	return err
}

func (e *Env) instruction(ctx context.Context, source, record, mandateID string, prop map[string]any) (map[string]any, error) {
	r := e.do(ctx, "POST", "/v1/instructions", "issuer-a", map[string]any{
		"source_system": source, "source_record_id": record + "-" + e.RunID, "mandate_id": mandateID, "proposed_action": prop})
	if r.Err != nil || r.Status != 201 {
		return nil, fmt.Errorf("create instruction: %d %s %v", r.Status, r.code(), r.Err)
	}
	return r.Body, nil
}

func (e *Env) evaluate(ctx context.Context, who, id string, rev int) resp {
	return e.do(ctx, "POST", "/v1/actions/evaluate", who, map[string]any{"instruction_id": id, "action_revision": rev})
}

func (e *Env) admitAt(ctx context.Context, base, who string, action any, jws string) resp {
	return e.call(ctx, base, "POST", "/v1/operations", who, map[string]any{"action": action, "permit_jws": jws})
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var c map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&c)
	return c
}

func (r *Recorder) ledger(ctx context.Context, e *Env, mandateID string, want Ledger, name string) error {
	got, err := e.ledger(ctx, mandateID)
	if err != nil {
		return r.expect(name, false, "ledger read failed: %v", err)
	}
	r.res.Ledger = got.String()
	ok := got.Available == want.Available && got.Reserved == want.Reserved && got.Committed == want.Committed &&
		got.Unresolved == 0 && got.Orders == want.Orders && got.Available+got.Reserved+got.Committed+got.Unresolved == got.Limit
	err = r.expect(name, ok, "%s", got)
	r.note("%s", got.Detail())
	return err
}

// ---- scenarios ----

// flagship: INR 10,00,000 allowance; two actors request INR 6,00,000 at the
// same moment; exactly one reserves; commit; retry returns the same order.
func flagship(ctx context.Context, e *Env, r *Recorder) error {
	m, err := e.mandate(ctx, tenLakh, []string{"agent-a", "agent-b"}, []string{"supplier-a"})
	if err != nil {
		return err
	}
	if err := r.expect("owner grants mandate", true, "%s limit %s for agent-a and agent-b", m, INR(tenLakh)); err != nil {
		return err
	}
	if err := e.assign(ctx, m, SourceA, "agent-a"); err != nil {
		return err
	}
	if err := e.assign(ctx, m, SourceB, "agent-b"); err != nil {
		return err
	}
	a, err := e.instruction(ctx, SourceA, "po-request-a", m, proposal("supplier-a", line("item-a", 3, 20_000_000)))
	if err != nil {
		return err
	}
	b, err := e.instruction(ctx, SourceB, "po-request-b", m, proposal("supplier-a", line("item-b", 2, 30_000_000)))
	if err != nil {
		return err
	}
	if err := r.expect("trusted workflows create two instructions", a["actor_id"] == "agent-a" && b["actor_id"] == "agent-b",
		"%s → %s, %s → %s (actors assigned by the server)", SourceA, a["actor_id"], SourceB, b["actor_id"]); err != nil {
		return err
	}

	// Both actors request at the same moment.
	type out struct {
		who string
		r   resp
	}
	results := make([]out, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, x := range []struct{ who, id string }{{"agent-a", a["instruction_id"].(string)}, {"agent-b", b["instruction_id"].(string)}} {
		wg.Add(1)
		go func(i int, who, id string) {
			defer wg.Done()
			<-start
			results[i] = out{who, e.evaluate(ctx, who, id, 1)}
		}(i, x.who, x.id)
	}
	close(start)
	wg.Wait()
	var winner out
	granted, denied := 0, 0
	for _, o := range results {
		switch {
		case o.r.Status == 201:
			granted++
			winner = o
			r.note("%s: PERMIT_ISSUED", o.who)
		case o.r.Status == 200 && o.r.code() == "CAPACITY_EXHAUSTED":
			denied++
			r.note("%s: DENY (%s)", o.who, o.r.code())
		default:
			r.note("%s: unexpected %d %s %v", o.who, o.r.Status, o.r.code(), o.r.Err)
		}
	}
	if err := r.expect("concurrent INR 6,00,000 requests", granted == 1 && denied == 1, "%d permit issued, %d denied for exhausted capacity", granted, denied); err != nil {
		return err
	}
	if err := r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Reserved: sixLakh}, "one reservation held"); err != nil {
		return err
	}

	action, jws := winner.r.Body["action"], winner.r.Body["permit_jws"].(string)
	first := e.admitAt(ctx, e.BaseURL, winner.who, action, jws)
	if err := r.expect("winner presents the exact action and permit", first.Status == 201, "HTTP %d, order %v", first.Status, targetOrder(first)); err != nil {
		return err
	}
	if err := r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "committed, nothing reserved"); err != nil {
		return err
	}
	again := e.admitAt(ctx, e.BaseURL, winner.who, action, jws)
	if err := r.expect("retry returns the same order", again.Status == 200 && targetOrder(again) == targetOrder(first),
		"HTTP %d, order %v", again.Status, targetOrder(again)); err != nil {
		return err
	}
	if err := r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "no second charge"); err != nil {
		return err
	}

	op := first.Body["operation_id"].(string)
	rec := e.do(ctx, "POST", "/v1/operations/"+op+"/reconcile", "observer-a", nil)
	if err := r.expect("observer reads the stored order back", rec.Status == 200 && rec.Body["observation"] == "MATCH",
		"observation %v (read-only role)", rec.Body["observation"]); err != nil {
		return err
	}
	ev := e.do(ctx, "GET", "/v1/operations/"+op+"/evidence", winner.who, nil)
	if err := r.expect("signed evidence exported", ev.Status == 200 && ev.Body["evidence"] == "AVAILABLE",
		"evidence %v, incomplete %v", ev.Body["evidence"], ev.Body["incomplete"]); err != nil {
		return err
	}
	if e.EvidenceDir != "" {
		b, _ := json.MarshalIndent(ev.Body, "", "  ")
		path := filepath.Join(e.EvidenceDir, "flagship-evidence.json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		r.note("evidence bundle written to %s", path)
	}
	return nil
}

func targetOrder(r resp) any {
	if t, ok := r.Body["target_ref"].(map[string]any); ok {
		return t["order_id"]
	}
	return nil
}

// staleSupplier: the protected supplier record changes after the permit is
// issued; admission is rejected until the issuer amends and the actor
// re-evaluates.
func staleSupplier(ctx context.Context, e *Env, r *Recorder) error {
	m, err := e.mandate(ctx, tenLakh, []string{"agent-a"}, []string{"supplier-a"})
	if err != nil {
		return err
	}
	if err := e.assign(ctx, m, SourceA, "agent-a"); err != nil {
		return err
	}
	inst, err := e.instruction(ctx, SourceA, "po-request-stale", m, proposal("supplier-a", line("item-a", 3, 20_000_000)))
	if err != nil {
		return err
	}
	id := inst["instruction_id"].(string)
	ev := e.evaluate(ctx, "agent-a", id, 1)
	if err := r.expect("permit issued for supplier revision", ev.Status == 201, "supplier-a revision %v", inst["action"].(map[string]any)["supplier_revision"]); err != nil {
		return err
	}
	var rev int64
	if err := e.Admin.QueryRow(ctx, `UPDATE target.suppliers SET revision = revision + 1 WHERE tenant_id = $1 AND supplier_id = 'supplier-a' RETURNING revision`, e.Tenant).Scan(&rev); err != nil {
		return err
	}
	r.note("supplier record changed out of band (e.g. new bank details): supplier-a is now revision %d", rev)
	adm := e.admitAt(ctx, e.BaseURL, "agent-a", ev.Body["action"], ev.Body["permit_jws"].(string))
	if err := r.expect("stale permit rejected", adm.Status == 409 && adm.code() == "SUPPLIER_REVISION_STALE", "HTTP %d %s", adm.Status, adm.code()); err != nil {
		return err
	}
	re := e.evaluate(ctx, "agent-a", id, 1)
	if err := r.expect("old revision cannot be re-permitted", re.Status == 409 && re.code() == "SUPPLIER_REVISION_STALE", "HTTP %d %s", re.Status, re.code()); err != nil {
		return err
	}
	am := e.do(ctx, "POST", "/v1/instructions/"+id+"/revisions", "issuer-a", map[string]any{
		"amendment_id": "rebind-" + e.RunID, "expected_action_revision": 1, "proposed_action": proposal("supplier-a", line("item-a", 3, 20_000_000))})
	if err := r.expect("issuer re-authorises via amendment", am.Status == 201, "revision %v binds supplier revision %v; released %v paise",
		am.Body["action_revision"], am.Body["action"].(map[string]any)["supplier_revision"], am.Body["released_minor"]); err != nil {
		return err
	}
	ev2 := e.evaluate(ctx, "agent-a", id, 2)
	jws2, _ := ev2.Body["permit_jws"].(string)
	adm2 := e.admitAt(ctx, e.BaseURL, "agent-a", ev2.Body["action"], jws2)
	if err := r.expect("new revision evaluated and committed", ev2.Status == 201 && adm2.Status == 201, "evaluate %d, admit %d", ev2.Status, adm2.Status); err != nil {
		return err
	}
	return r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "one order at the current supplier revision")
}

// lostResponse: the order commits but the network drops the response. The
// client is uncertain; the database is not. Lookup and retry return the
// original order without a second charge.
func lostResponse(ctx context.Context, e *Env, r *Recorder) error {
	m, err := e.mandate(ctx, tenLakh, []string{"agent-a"}, []string{"supplier-a"})
	if err != nil {
		return err
	}
	if err := e.assign(ctx, m, SourceA, "agent-a"); err != nil {
		return err
	}
	inst, err := e.instruction(ctx, SourceA, "po-request-lost", m, proposal("supplier-a", line("item-a", 3, 20_000_000)))
	if err != nil {
		return err
	}
	ev := e.evaluate(ctx, "agent-a", inst["instruction_id"].(string), 1)
	if err := r.expect("permit issued", ev.Status == 201, "operation %v", ev.Body["permit"].(map[string]any)["operation_id"]); err != nil {
		return err
	}
	op := ev.Body["permit"].(map[string]any)["operation_id"].(string)

	proxy, upstreamStatus, stop, err := droppingProxy(e.BaseURL)
	if err != nil {
		return err
	}
	defer stop()
	lost := e.admitAt(ctx, proxy, "agent-a", ev.Body["action"], ev.Body["permit_jws"].(string))
	if err := r.expect("response lost in transit", lost.Err != nil && lost.Status == 0,
		"client saw: %v (the server had answered HTTP %d)", lost.Err, upstreamStatus()); err != nil {
		return err
	}
	if err := r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "database truth: committed once"); err != nil {
		return err
	}
	look := e.do(ctx, "GET", "/v1/operations/"+op, "agent-a", nil)
	if err := r.expect("client looks up the operation", look.Status == 200 && look.Body["execution"] == "COMMITTED", "execution %v", look.Body["execution"]); err != nil {
		return err
	}
	retry := e.admitAt(ctx, e.BaseURL, "agent-a", ev.Body["action"], ev.Body["permit_jws"].(string))
	if err := r.expect("retry returns the original result", retry.Status == 200 && retry.Body["operation_id"] == op, "HTTP %d operation %v", retry.Status, retry.Body["operation_id"]); err != nil {
		return err
	}
	return r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "no release, no second charge")
}

// droppingProxy forwards requests to upstream, waits for the full upstream
// response, then drops the client connection without sending it.
func droppingProxy(upstream string) (url string, lastStatus func() int, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	var mu sync.Mutex
	status := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		out, _ := http.NewRequestWithContext(context.WithoutCancel(req.Context()), req.Method, upstream+req.URL.RequestURI(), bytes.NewReader(body))
		out.Header = req.Header.Clone()
		res, err := http.DefaultClient.Do(out)
		if err == nil {
			_, _ = io.ReadAll(res.Body)
			res.Body.Close()
			mu.Lock()
			status = res.StatusCode
			mu.Unlock()
		}
		panic(http.ErrAbortHandler) // drop the connection: the response never reaches the client
	}), ErrorLog: nil}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() int { mu.Lock(); defer mu.Unlock(); return status },
		func() { _ = srv.Close() }, nil
}

// agentInjection: untrusted supplier text tells the agent to switch supplier,
// inflate the order, raise its allowance and create more orders. The agent
// here is a deterministic stand-in that obeys the text; no model is called.
// Every injected request is rejected by the deterministic gate.
func agentInjection(ctx context.Context, e *Env, r *Recorder) error {
	m, err := e.mandate(ctx, tenLakh, []string{"agent-a"}, []string{"supplier-a"})
	if err != nil {
		return err
	}
	if err := e.assign(ctx, m, SourceA, "agent-a"); err != nil {
		return err
	}
	inst, err := e.instruction(ctx, SourceA, "po-request-inject", m, proposal("supplier-a", line("item-a", 3, 20_000_000)))
	if err != nil {
		return err
	}
	id := inst["instruction_id"].(string)
	ev := e.evaluate(ctx, "agent-a", id, 1)
	if err := r.expect("permit issued for the authorised order", ev.Status == 201, "3 × item-a from supplier-a, %s", INR(sixLakh)); err != nil {
		return err
	}
	r.note("untrusted supplier message (fixtures/demo/supplier-message.txt) instructs the agent to:")
	r.note("switch to supplier-b, raise quantity to 15, raise its allowance to INR 1,00,00,000, create more orders")
	r.note("the stand-in agent obeys every instruction (simulated compromised agent; no model is called)")
	jws := ev.Body["permit_jws"].(string)

	switched := clone(ev.Body["action"].(map[string]any))
	switched["supplier_id"] = "supplier-b"
	a1 := e.admitAt(ctx, e.BaseURL, "agent-a", switched, jws)
	if err := r.expect("switch supplier with the issued permit", a1.Status == 409 && a1.code() == "ACTION_CHANGED", "HTTP %d %s", a1.Status, a1.code()); err != nil {
		return err
	}
	inflated := clone(ev.Body["action"].(map[string]any))
	inflated["lines"] = []any{line("item-a", 15, 20_000_000)}
	inflated["amount_minor"] = 300_000_000
	a2 := e.admitAt(ctx, e.BaseURL, "agent-a", inflated, jws)
	if err := r.expect("inflate the order to INR 30,00,000", a2.Status == 409 && a2.code() == "ACTION_CHANGED", "HTTP %d %s", a2.Status, a2.code()); err != nil {
		return err
	}
	a3 := e.do(ctx, "POST", "/v1/mandates", "agent-a", map[string]any{"actor_ids": []string{"agent-a"}, "audience": "synthetic-procurement",
		"supplier_ids": []string{"supplier-b"}, "delivery_ids": []string{"warehouse-a"}, "currency": "INR", "limit_minor": 1_000_000_000,
		"expires_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")})
	if err := r.expect("raise its own allowance", a3.Status == 403 && a3.code() == "FORBIDDEN", "HTTP %d %s", a3.Status, a3.code()); err != nil {
		return err
	}
	a4 := e.do(ctx, "POST", "/v1/instructions", "agent-a", map[string]any{"source_system": SourceA, "source_record_id": "injected-" + e.RunID,
		"mandate_id": m, "proposed_action": proposal("supplier-b", line("item-a", 15, 20_000_000))})
	if err := r.expect("create a follow-up order itself", a4.Status == 403 && a4.code() == "FORBIDDEN", "HTTP %d %s", a4.Status, a4.code()); err != nil {
		return err
	}
	a5 := e.do(ctx, "POST", "/v1/instructions/"+id+"/revisions", "agent-a", map[string]any{"amendment_id": "injected-" + e.RunID,
		"expected_action_revision": 1, "proposed_action": proposal("supplier-b", line("item-a", 15, 20_000_000))})
	if err := r.expect("amend the instruction to supplier-b", a5.Status == 403 && a5.code() == "FORBIDDEN", "HTTP %d %s", a5.Status, a5.code()); err != nil {
		return err
	}
	if err := r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Reserved: sixLakh}, "no unauthorised effect"); err != nil {
		return err
	}
	ok := e.admitAt(ctx, e.BaseURL, "agent-a", ev.Body["action"], jws)
	if err := r.expect("only the exact authorised order can commit", ok.Status == 201, "HTTP %d, supplier-a, %s", ok.Status, INR(sixLakh)); err != nil {
		return err
	}
	return r.ledger(ctx, e, m, Ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1}, "one authorised order")
}
