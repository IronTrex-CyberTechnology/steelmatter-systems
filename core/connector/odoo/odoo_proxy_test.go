//go:build odoo

// Lost replies on the real network path: connector -> lossproxy -> Odoo
// sandbox. Each test shows from the proxy's log that the failure really
// happened (Odoo applied the call, or never saw it), then that the connector
// reported it as unknown, never as proof, and that lookup and fence settle it.
package odoo_test

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/tools/lossproxy"
)

// throughProxy returns a connector whose traffic goes through a fresh
// in-process lossproxy in front of the sandbox.
func throughProxy(t *testing.T, timeout time.Duration) (*odoo.Connector, *lossproxy.Proxy) {
	t.Helper()
	sandbox(t) // skips without the sandbox
	p, err := lossproxy.New(os.Getenv("STEELMATTER_ODOO_URL"))
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	c, err := odoo.New(odoo.Config{
		URL: front.URL, Database: os.Getenv("STEELMATTER_ODOO_DB"), Instance: "via-proxy", Timeout: timeout,
		Executor: odoo.Credentials{Login: "sm_executor", Password: os.Getenv("STEELMATTER_ODOO_EXECUTOR_PASSWORD")},
		Observer: odoo.Credentials{Login: "sm_observer", Password: os.Getenv("STEELMATTER_ODOO_OBSERVER_PASSWORD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func mustFault(t *testing.T, p *lossproxy.Proxy, f lossproxy.Fault) {
	t.Helper()
	if err := p.Add(f); err != nil {
		t.Fatal(err)
	}
}

// faulted returns the proxy's record of the faulted method call for op.
func faulted(t *testing.T, p *lossproxy.Proxy, op, method string) lossproxy.Event {
	t.Helper()
	for _, e := range p.Events(op) {
		if e.Method == method && e.Action != "forwarded" {
			return e
		}
	}
	t.Fatalf("no faulted %s call for %s in %+v", method, op, p.Events(op))
	return lossproxy.Event{}
}

func createdRows(t *testing.T, c *odoo.Connector, op string) int {
	t.Helper()
	n := 0
	var cp int64
	for {
		page, err := c.List(context.Background(), cp, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) == 0 {
			return n
		}
		for _, r := range page.Rows {
			if r.OpRef == op && r.State == "created" && r.OrderID != "" {
				n++
			}
		}
		cp = page.NextCheckpoint
	}
}

func requireUnknown(t *testing.T, r connector.Result) {
	t.Helper()
	if r.Outcome != connector.Unknown || r.ProvesNoOrder() || r.BlockedOverPriorCreate() || r.Destination != nil {
		t.Fatalf("a lost reply must be UNKNOWN with no proof: %+v", r)
	}
}

func TestLostCreateReplyOnTheNetwork(t *testing.T) {
	c, p := throughProxy(t, 30*time.Second)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	op := opRef("net-drop-reply")
	mustFault(t, p, lossproxy.Fault{Mode: lossproxy.DropReply, Method: "sm_create", OpRef: op})

	r, err := c.Execute(ctx, action(op, rev))
	if err != nil {
		t.Fatal(err)
	}
	requireUnknown(t, r)
	if e := faulted(t, p, op, "sm_create"); !e.Forwarded || e.UpstreamOutcome != string(connector.Created) {
		t.Fatalf("the proxy must show Odoo created the order: %+v", e)
	}
	lk, _ := c.Lookup(ctx, op)
	if lk.Outcome != connector.Found || lk.Destination == nil {
		t.Fatalf("lookup %+v", lk)
	}
	retry, _ := c.Execute(ctx, action(op, rev))
	if retry.Outcome != connector.Exists || retry.Destination.ID != lk.Destination.ID {
		t.Fatalf("retry %+v", retry)
	}
	if n := createdRows(t, c, op); n != 1 {
		t.Fatalf("%d orders for %s, want 1", n, op)
	}
}

func TestCreateLostBeforeReachingOdoo(t *testing.T) {
	c, p := throughProxy(t, 30*time.Second)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	op := opRef("net-drop-request")
	mustFault(t, p, lossproxy.Fault{Mode: lossproxy.DropRequest, Method: "sm_create", OpRef: op})

	r, _ := c.Execute(ctx, action(op, rev))
	requireUnknown(t, r)
	if e := faulted(t, p, op, "sm_create"); e.Forwarded {
		t.Fatalf("the request reached Odoo: %+v", e)
	}
	if lk, _ := c.Lookup(ctx, op); lk.Outcome != connector.NotFoundNow || lk.ProvesNoOrder() {
		t.Fatalf("lookup %+v: not found is never proof", lk)
	}
	f, _ := c.Fence(ctx, op, 0)
	if f.Outcome != connector.Fenced || !f.ProvesNoOrder() {
		t.Fatalf("fence %+v", f)
	}
	if late, _ := c.Execute(ctx, action(op, rev)); late.Outcome != connector.RefusedFenced {
		t.Fatalf("late create %+v", late)
	}
	if n := createdRows(t, c, op); n != 0 {
		t.Fatalf("%d orders for %s, want 0", n, op)
	}
}

func TestReplyDelayedPastTheConnectorTimeout(t *testing.T) {
	c, p := throughProxy(t, 1*time.Second)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	op := opRef("net-delay")
	mustFault(t, p, lossproxy.Fault{Mode: lossproxy.DelayReply, Method: "sm_create", OpRef: op, DelayMS: 3000})

	r, _ := c.Execute(ctx, action(op, rev))
	requireUnknown(t, r)
	// Odoo finished the create; only the reply was late.
	deadline := time.Now().Add(10 * time.Second)
	for {
		lk, _ := c.Lookup(ctx, op)
		if lk.Outcome == connector.Found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("order never found: %+v", lk)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if e := faulted(t, p, op, "sm_create"); e.UpstreamOutcome != string(connector.Created) {
		t.Fatalf("proxy event %+v", e)
	}
	if n := createdRows(t, c, op); n != 1 {
		t.Fatalf("%d orders, want 1", n)
	}
}

func TestBadGatewayAfterOdooCreated(t *testing.T) {
	c, p := throughProxy(t, 30*time.Second)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	op := opRef("net-502")
	mustFault(t, p, lossproxy.Fault{Mode: lossproxy.BadGateway, Method: "sm_create", OpRef: op})

	r, _ := c.Execute(ctx, action(op, rev))
	requireUnknown(t, r)
	if e := faulted(t, p, op, "sm_create"); e.UpstreamOutcome != string(connector.Created) {
		t.Fatalf("proxy event %+v", e)
	}
	if lk, _ := c.Lookup(ctx, op); lk.Outcome != connector.Found {
		t.Fatalf("lookup %+v", lk)
	}
}

func TestLostFenceReplyThenFenceAgain(t *testing.T) {
	c, p := throughProxy(t, 30*time.Second)
	ctx := context.Background()
	op := opRef("net-fence")
	mustFault(t, p, lossproxy.Fault{Mode: lossproxy.DropReply, Method: "sm_fence", OpRef: op})

	r, _ := c.Fence(ctx, op, 0)
	requireUnknown(t, r)
	if e := faulted(t, p, op, "sm_fence"); e.UpstreamOutcome != string(connector.Fenced) {
		t.Fatalf("the proxy must show Odoo fenced: %+v", e)
	}
	again, _ := c.Fence(ctx, op, 0)
	if again.Outcome != connector.Fenced || !again.ProvesNoOrder() {
		t.Fatalf("fence again %+v", again)
	}
}
