//go:build integration && failpoints && odoo

// Engine plus real Odoo with the reply lost on the network (T33), not inside
// the connector: connector -> lossproxy -> Odoo sandbox. Run through
// destinations/odoo/sandbox/test-engine on a fresh sandbox, like
// TestR1OdooEndToEnd, whose harness this reuses.

package integration

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/tools/lossproxy"
)

func TestR1OdooThroughLossProxy(t *testing.T) {
	r := setupOdoo(t)
	px, err := lossproxy.New(os.Getenv("STEELMATTER_ODOO_URL"))
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(px)
	t.Cleanup(front.Close)
	conn, err := odoo.New(odoo.Config{URL: front.URL, Database: os.Getenv("STEELMATTER_ODOO_DB"), Instance: "via-proxy", Timeout: 2 * time.Second,
		Executor: odoo.Credentials{Login: "sm_executor", Password: os.Getenv("STEELMATTER_ODOO_EXECUTOR_PASSWORD")},
		Observer: odoo.Credentials{Login: "sm_observer", Password: os.Getenv("STEELMATTER_ODOO_OBSERVER_PASSWORD")}})
	if err != nil {
		t.Fatal(err)
	}
	r.e.eng.Connector = conn
	const sixL = int64(6_000_000)

	held := func(base buckets) buckets {
		return buckets{Available: base.Available - sixL, Reserved: base.Reserved, Committed: base.Committed, Unresolved: base.Unresolved + sixL}
	}
	createEvent := func(t *testing.T, op string) lossproxy.Event {
		t.Helper()
		for _, e := range px.Events(op) {
			if e.Method == "sm_create" && e.Action != "forwarded" {
				return e
			}
		}
		t.Fatalf("no faulted sm_create for %s: %+v", op, px.Events(op))
		return lossproxy.Event{}
	}

	t.Run("T33 on the network: Odoo created the order, the reply was dropped; held, then found and committed once", func(t *testing.T) {
		base := r.e.buckets(r.mandate)
		p := r.order(t)
		mustDispatch(t, r.e, p)
		if err := px.Add(lossproxy.Fault{Mode: lossproxy.DropReply, Method: "sm_create", OpRef: p.operation}); err != nil {
			t.Fatal(err)
		}
		requireExecution(t, "reply lost", r.settle(t, p.operation), engine.DispatchUnresolved)
		if e := createEvent(t, p.operation); !e.Forwarded || e.UpstreamOutcome != string(connector.Created) {
			t.Fatalf("the proxy must show Odoo created the order: %+v", e)
		}
		requireBuckets(t, "held while unknown", r.e.buckets(r.mandate), held(base))
		requireExecution(t, "found", r.settle(t, p.operation), engine.DispatchCommitted)
		r.outcome(t, p.operation, base, 1, sixL, 0)
		r.evidence(t, p.operation)
	})

	t.Run("request lost on the network before Odoo: held, fenced and released; no order", func(t *testing.T) {
		base := r.e.buckets(r.mandate)
		p := r.order(t)
		mustDispatch(t, r.e, p)
		if err := px.Add(lossproxy.Fault{Mode: lossproxy.DropRequest, Method: "sm_create", OpRef: p.operation}); err != nil {
			t.Fatal(err)
		}
		requireExecution(t, "request lost", r.settle(t, p.operation), engine.DispatchUnresolved)
		if e := createEvent(t, p.operation); e.Forwarded {
			t.Fatalf("the request reached Odoo: %+v", e)
		}
		requireBuckets(t, "held while unknown", r.e.buckets(r.mandate), held(base))
		requireExecution(t, "fenced", r.settle(t, p.operation), engine.DispatchFenced)
		r.outcome(t, p.operation, base, 0, 0, 0)
		r.evidence(t, p.operation)
	})

	t.Run("reply delayed past the connector timeout: held, then found and committed once", func(t *testing.T) {
		base := r.e.buckets(r.mandate)
		p := r.order(t)
		mustDispatch(t, r.e, p)
		if err := px.Add(lossproxy.Fault{Mode: lossproxy.DelayReply, Method: "sm_create", OpRef: p.operation, DelayMS: 5000}); err != nil {
			t.Fatal(err)
		}
		requireExecution(t, "timed out", r.settle(t, p.operation), engine.DispatchUnresolved)
		requireBuckets(t, "held while unknown", r.e.buckets(r.mandate), held(base))
		requireExecution(t, "found", r.settle(t, p.operation), engine.DispatchCommitted)
		if e := createEvent(t, p.operation); e.UpstreamOutcome != string(connector.Created) {
			t.Fatalf("proxy event %+v", e)
		}
		r.outcome(t, p.operation, base, 1, sixL, 0)
		r.evidence(t, p.operation)
	})
}
