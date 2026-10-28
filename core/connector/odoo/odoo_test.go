package odoo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
)

const (
	execPW = "exec-secret-0f3a"
	obsPW  = "obs-secret-9b2c"
)

// fakeOdoo answers JSON-RPC like Odoo: common.login and
// object.execute_kw on steelmatter.api, with a handler per method.
type fakeOdoo struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string // "login:method"
	handlers map[string]func(args []json.RawMessage) (any, *rpcErr)
	raw      func(w http.ResponseWriter) bool // optional: replace the whole reply
}

type rpcErr struct{ name string }

func newFake(t *testing.T) (*fakeOdoo, *Connector) {
	f := &fakeOdoo{t: t, handlers: map[string]func([]json.RawMessage) (any, *rpcErr){}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, Database: "smr1", Executor: Credentials{"sm_executor", execPW},
		Observer: Credentials{"sm_observer", obsPW}, Timeout: 2 * time.Second, Instance: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeOdoo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.raw != nil && f.raw(w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Params struct {
			Service string            `json:"service"`
			Method  string            `json:"method"`
			Args    []json.RawMessage `json:"args"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("bad request: %v", err)
		return
	}
	reply := func(result any, e *rpcErr) {
		out := map[string]any{"jsonrpc": "2.0", "id": 1}
		if e != nil {
			out["error"] = map[string]any{"code": 200, "message": "Odoo Server Error", "data": map[string]any{"name": e.name}}
		} else {
			out["result"] = result
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	a := req.Params.Args
	if req.Params.Service == "common" && req.Params.Method == "login" {
		var login, pw string
		_ = json.Unmarshal(a[1], &login)
		_ = json.Unmarshal(a[2], &pw)
		switch {
		case login == "sm_executor" && pw == execPW:
			reply(6, nil)
		case login == "sm_observer" && pw == obsPW:
			reply(7, nil)
		default:
			reply(false, nil)
		}
		return
	}
	var uid int
	var model, method string
	_ = json.Unmarshal(a[1], &uid)
	_ = json.Unmarshal(a[3], &model)
	_ = json.Unmarshal(a[4], &method)
	var args []json.RawMessage
	_ = json.Unmarshal(a[5], &args)
	who := map[int]string{6: "sm_executor", 7: "sm_observer"}[uid]
	f.mu.Lock()
	f.calls = append(f.calls, who+":"+method)
	h := f.handlers[model+"."+method]
	f.mu.Unlock()
	if h == nil {
		reply(nil, &rpcErr{"odoo.exceptions.UserError"})
		return
	}
	reply(h(args))
}

func (f *fakeOdoo) on(method string, h func(args []json.RawMessage) (any, *rpcErr)) {
	f.mu.Lock()
	f.handlers["steelmatter.api."+method] = h
	f.mu.Unlock()
}

func (f *fakeOdoo) callsMade() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func order(opRef string) map[string]any {
	return map[string]any{"id": "P00001", "state": "purchase", "company_ref": "acme-in", "delivery_ref": "wh-chennai",
		"supplier_ref": "sup-chairs", "lines": []any{map[string]any{"item_ref": "chair-ergo", "quantity": 10, "unit_price_minor": 600000}},
		"amount_minor": 6000000, "currency": "INR", "permit_digest": "sha256:" + opRef}
}

func result(opRef, call, outcome string, extra map[string]any) map[string]any {
	r := map[string]any{"op_ref": opRef, "call": call, "outcome": outcome, "reason": "", "seq": 3, "incarnation": "abc",
		"odoo_tx_time": "2026-10-28T10:00:00.123456Z", "connector_received_at": nil, "source": "odoo/steelmatter_r1@18.0.0.1.0/executor",
		"destination": nil, "block_committed": false, "prior_create": false, "create_done": false}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func act(opRef string) connector.Action {
	return connector.Action{OpRef: opRef, ActionRevision: 1, CompanyRef: "acme-in", DeliveryRef: "wh-chennai", SupplierRef: "sup-chairs",
		SupplierRevision: 1, Lines: []connector.Line{{ItemRef: "chair-ergo", Quantity: 10, UnitPriceMinor: 600000}},
		AmountMinor: 6000000, Currency: "INR", PermitDigest: "sha256:" + opRef}
}

func noSecrets(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	s := string(b) + fmt.Sprint(v)
	if strings.Contains(s, execPW) || strings.Contains(s, obsPW) {
		t.Fatalf("credential leaked: %s", s)
	}
}

func TestExecuteSendsOnlyTheCanonicalActionAsExecutor(t *testing.T) {
	f, c := newFake(t)
	var sent map[string]any
	f.on("sm_create", func(args []json.RawMessage) (any, *rpcErr) {
		_ = json.Unmarshal(args[0], &sent)
		return result("op-1", "execute", "CREATED", map[string]any{"destination": order("op-1"), "prior_create": true}), nil
	})
	r, err := c.Execute(context.Background(), act("op-1"))
	if err != nil || r.Outcome != connector.Created || r.Destination == nil || r.Destination.ID != "P00001" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if r.ConnectorReceivedAt.IsZero() || r.OdooTxTime.IsZero() || !strings.HasPrefix(r.Source, "test/executor ") {
		t.Fatalf("connector fields not set: %+v", r)
	}
	if r.PriorCreate == nil || !*r.PriorCreate || r.BlockCommitted == nil || *r.BlockCommitted {
		t.Fatalf("flags: %+v", r)
	}
	want := map[string]bool{"op_ref": true, "action_revision": true, "company_ref": true, "delivery_ref": true, "supplier_ref": true,
		"supplier_revision": true, "lines": true, "amount_minor": true, "currency": true, "permit_digest": true, "expected_min_seq": true}
	for k := range sent {
		if !want[k] {
			t.Fatalf("execute sent a field outside the canonical action: %s", k)
		}
	}
	if got := f.callsMade(); len(got) != 1 || got[0] != "sm_executor:sm_create" {
		t.Fatalf("calls: %v", got)
	}
	noSecrets(t, r)
}

func TestReadsUseTheObserverAndNeverTheExecutor(t *testing.T) {
	f, c := newFake(t)
	f.on("sm_lookup", func([]json.RawMessage) (any, *rpcErr) {
		return result("op-1", "lookup", "FOUND", map[string]any{"destination": order("op-1")}), nil
	})
	f.on("sm_observe", func([]json.RawMessage) (any, *rpcErr) {
		return result("op-1", "observe", "MATCH", map[string]any{"destination": order("op-1")}), nil
	})
	f.on("sm_list_since", func([]json.RawMessage) (any, *rpcErr) {
		return map[string]any{"rows": []any{map[string]any{"seq": 1, "op_ref": "op-1", "state": "created", "event": "created"}},
			"next_checkpoint": 1, "head_seq": 1, "incarnation": "abc"}, nil
	})
	ctx := context.Background()
	if r, _ := c.Lookup(ctx, "op-1"); r.Outcome != connector.Found {
		t.Fatalf("lookup %+v", r)
	}
	if r, _ := c.Observe(ctx, act("op-1")); r.Outcome != connector.Match {
		t.Fatalf("observe %+v", r)
	}
	if p, err := c.List(ctx, 0, 10); err != nil || len(p.Rows) != 1 || p.HeadSeq != 1 {
		t.Fatalf("list %+v %v", p, err)
	}
	for _, call := range f.callsMade() {
		if !strings.HasPrefix(call, "sm_observer:") {
			t.Fatalf("read call used the executor: %s", call)
		}
	}
}

func TestFailuresAreNeverProof(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *fakeOdoo)
		reason string
	}{
		{"odoo exception", func(f *fakeOdoo) {
			for _, m := range []string{"sm_create", "sm_fence", "sm_lookup"} {
				f.on(m, func([]json.RawMessage) (any, *rpcErr) { return nil, &rpcErr{"psycopg2.errors.SerializationFailure"} })
			}
		}, ReasonDestinationError},
		{"malformed reply", func(f *fakeOdoo) {
			f.raw = func(w http.ResponseWriter) bool { _, _ = io.WriteString(w, "<html>proxy error</html>"); return true }
		}, ReasonMalformedReply},
		{"http 502", func(f *fakeOdoo) {
			f.raw = func(w http.ResponseWriter) bool { w.WriteHeader(http.StatusBadGateway); return true }
		}, connector.ReasonDestinationUnavailable},
		{"reply for another op_ref", func(f *fakeOdoo) {
			for _, m := range []string{"sm_create", "sm_fence", "sm_lookup"} {
				f.on(m, func([]json.RawMessage) (any, *rpcErr) {
					return result("op-other", "execute", "FENCED", map[string]any{"block_committed": true}), nil
				})
			}
		}, ReasonMalformedReply},
		{"CREATED or FOUND without order facts", func(f *fakeOdoo) {
			f.on("sm_create", func([]json.RawMessage) (any, *rpcErr) { return result("op-1", "execute", "CREATED", nil), nil })
			f.on("sm_fence", func([]json.RawMessage) (any, *rpcErr) { return nil, &rpcErr{"x"} })
			f.on("sm_lookup", func([]json.RawMessage) (any, *rpcErr) { return result("op-1", "lookup", "FOUND", nil), nil })
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t)
			tc.setup(f)
			ctx := context.Background()
			ex, err1 := c.Execute(ctx, act("op-1"))
			fe, err2 := c.Fence(ctx, "op-1", 0)
			lk, err3 := c.Lookup(ctx, "op-1")
			if err1 != nil || err2 != nil || err3 != nil {
				t.Fatalf("a destination failure must be a result, not an error: %v %v %v", err1, err2, err3)
			}
			if ex.Outcome != connector.Unknown || fe.Outcome != connector.Unknown || lk.Outcome != connector.Unavailable {
				t.Fatalf("execute %s, fence %s, lookup %s", ex.Outcome, fe.Outcome, lk.Outcome)
			}
			for _, r := range []connector.Result{ex, fe, lk} {
				if r.ProvesNoOrder() || r.BlockedOverPriorCreate() || r.Destination != nil {
					t.Fatalf("failure carried proof: %+v", r)
				}
				if tc.reason != "" && r.Reason != tc.reason {
					t.Fatalf("reason %q, want %q", r.Reason, tc.reason)
				}
				noSecrets(t, r)
			}
		})
	}
}

func TestTimeoutAfterSendIsUnknown(t *testing.T) {
	f, c := newFake(t)
	c.timeout = 100 * time.Millisecond
	f.on("sm_create", func([]json.RawMessage) (any, *rpcErr) {
		time.Sleep(400 * time.Millisecond) // the create may have committed; the reply is lost
		return result("op-1", "execute", "CREATED", map[string]any{"destination": order("op-1")}), nil
	})
	r, err := c.Execute(context.Background(), act("op-1"))
	if err != nil || r.Outcome != connector.Unknown || r.Reason != connector.ReasonDestinationUnavailable {
		t.Fatalf("got %+v %v", r, err)
	}
}

func TestUnreachableDestinationIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, err := New(Config{URL: url, Database: "smr1", Executor: Credentials{"e", execPW}, Observer: Credentials{"o", obsPW}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.Execute(context.Background(), act("op-1"))
	if r.Outcome != connector.Unknown {
		t.Fatalf("got %+v", r)
	}
	rd, _ := c.Readiness(context.Background(), act("op-1"))
	if rd.Outcome != connector.NotReady || rd.Reason != connector.ReasonDestinationUnavailable {
		t.Fatalf("readiness %+v", rd)
	}
}

func TestRefusedLoginAndRotation(t *testing.T) {
	f, c := newFake(t)
	f.on("sm_fence", func([]json.RawMessage) (any, *rpcErr) {
		return result("op-1", "fence", "FENCED", map[string]any{"block_committed": true}), nil
	})
	if err := c.SetCredentials(Credentials{"sm_executor", "wrong"}, Credentials{"sm_observer", obsPW}); err != nil {
		t.Fatal(err)
	}
	r, _ := c.Fence(context.Background(), "op-1", 0)
	if r.Outcome != connector.Unknown || r.Reason != connector.ReasonCredentialDisabled {
		t.Fatalf("got %+v", r)
	}
	noSecrets(t, r)
	if err := c.SetCredentials(Credentials{"sm_executor", execPW}, Credentials{"sm_observer", obsPW}); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Fence(context.Background(), "op-1", 0); r.Outcome != connector.Fenced || !r.ProvesNoOrder() {
		t.Fatalf("after rotation %+v", r)
	}
	if err := c.SetCredentials(Credentials{"same", "a"}, Credentials{"same", "b"}); err == nil {
		t.Fatal("one login for both identities must be refused")
	}
}

func TestReadinessChecksTheOdooVersion(t *testing.T) {
	f, c := newFake(t)
	f.on("sm_version", func([]json.RawMessage) (any, *rpcErr) {
		return map[string]any{"module": "18.0.0.1.0", "odoo": "17.0-20240101", "supported": false}, nil
	})
	r, _ := c.Readiness(context.Background(), act("op-1"))
	if r.Outcome != connector.NotReady || r.Reason != connector.ReasonVersionUnsupported {
		t.Fatalf("got %+v", r)
	}
}

func TestListRejectsRowsOutOfOrder(t *testing.T) {
	f, c := newFake(t)
	f.on("sm_list_since", func([]json.RawMessage) (any, *rpcErr) {
		return map[string]any{"rows": []any{map[string]any{"seq": 5, "state": "created"}, map[string]any{"seq": 4, "state": "fenced"}},
			"next_checkpoint": 4, "head_seq": 5}, nil
	})
	if _, err := c.List(context.Background(), 0, 10); err == nil {
		t.Fatal("out-of-order rows must be an error")
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	good := Config{URL: "https://odoo.example", Database: "d", Executor: Credentials{"e", "p"}, Observer: Credentials{"o", "q"}}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"plain http to a remote host": func(c *Config) { c.URL = "http://odoo.example:8069" },
		"same identity":               func(c *Config) { c.Observer = c.Executor },
		"missing observer":            func(c *Config) { c.Observer = Credentials{} },
		"missing database":            func(c *Config) { c.Database = "" },
	} {
		cfg := good
		mut(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	local := good
	local.URL = "http://127.0.0.1:18069"
	if _, err := New(local); err != nil {
		t.Fatalf("loopback http: %v", err)
	}
}

func TestCoverageDecodesAndNeverClaimsCompleteness(t *testing.T) {
	f, c := newFake(t)
	f.on("sm_coverage", func([]json.RawMessage) (any, *rpcErr) {
		return map[string]any{
			"generated_at": "2026-10-28T10:00:00Z", "database": "smr1", "seq": 9, "incarnation": "abc",
			"module": map[string]any{"name": "steelmatter_r1", "installed": true, "version": "18.0.0.1.0", "expected_version": "18.0.0.1.0",
				"odoo": "18.0-20261015", "supported": true},
			"anchor": map[string]any{"configured": true, "reachable": true},
			"routes": []any{
				map[string]any{"id": "steelmatter_api", "route": "r", "posture": "enforced", "detail": "d"},
				map[string]any{"id": "direct_users", "route": "r", "posture": "unprotected", "detail": "d", "count": 1,
					"subjects": []any{map[string]any{"login": "admin"}}},
				map[string]any{"id": "odd", "route": "r", "posture": "fully-safe", "detail": "d"},
			},
			"summary": map[string]any{"enforced": 2}, "complete": true,
		}, nil
	})
	cov := c.Coverage(context.Background())
	if !cov.Verified || cov.Complete || cov.Module.Version != "18.0.0.1.0" || len(cov.Routes) != 3 {
		t.Fatalf("got %+v", cov)
	}
	if cov.Routes[2].Posture != PostureUnknown {
		t.Fatalf("an unrecognised posture must become unknown: %+v", cov.Routes[2])
	}
	if cov.Summary[PostureEnforced] != 1 || cov.Summary[PostureUnprotected] != 1 || cov.Summary[PostureUnknown] != 1 {
		t.Fatalf("summary must be recounted by the connector: %v", cov.Summary)
	}
	if got := f.callsMade(); len(got) != 1 || got[0] != "sm_observer:sm_coverage" {
		t.Fatalf("coverage must use the observer: %v", got)
	}
}

func TestCoverageWithoutTheModuleIsUnverified(t *testing.T) {
	_, c := newFake(t) // no sm_coverage handler: Odoo answers with an error, as when the module is missing
	cov := c.Coverage(context.Background())
	if cov.Verified || cov.Complete || len(cov.Routes) != 1 || cov.Routes[0].Posture != PostureUnknown || cov.Summary[PostureUnknown] != 1 {
		t.Fatalf("got %+v", cov)
	}
	noSecrets(t, cov)
}
