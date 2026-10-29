//go:build integration && failpoints

package integration

import (
	"context"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// R1-DEF-13: an order edited or cancelled in Odoo after it was committed is
// flagged on the operation (ORDER_CHANGED_AT_DESTINATION): from the
// destination journal by the restore check, and from the observer's signed
// readback. It stays committed with no refund, writes stay on, and an
// operator can record a finding. A later deletion replaces the flag.
func TestR1DEF13ChangedOrderIsFlagged(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*env, *anchored, permitted)
	}{
		{"edited in Odoo", func(e *env, a *anchored, p permitted) {
			if !a.s.EditOrder(p.operation, func(o *connector.Order) { o.Lines[0].UnitPriceMinor++; o.AmountMinor += o.Lines[0].Quantity }) {
				e.t.Fatal("edit")
			}
		}},
		{"cancelled in Odoo", func(e *env, a *anchored, p permitted) {
			if !a.s.CancelOrder(p.operation) {
				e.t.Fatal("cancel")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newR1Env(t)
			a := withAnchor(t, e)
			p := e.permitted()
			mustDispatch(t, e, p)
			requireExecution(t, "commit", e.settle(p.operation, a.s), engine.DispatchCommitted)
			c.change(e, a, p)
			r := a.check()
			if r.WritesDisabled || len(r.Signals) != 0 {
				t.Fatalf("a change in Odoo was taken for a restore: %+v", r)
			}
			if len(r.Flagged) != 1 || r.Flagged[0] != p.operation {
				t.Fatalf("not flagged: %+v", r)
			}
			st, body := e.call("GET", "/v1/dispatches/"+p.operation, "observer-a", nil)
			requireStatus(t, "dispatch", st, body, 200, "")
			if body["execution"] != engine.DispatchCommitted || body["flag"] != engine.FlagOrderChangedAtDestination {
				t.Fatalf("dispatch %v", body)
			}
			requireBuckets(t, "no refund", e.buckets(p.mandate), r1Committed)
			if _, err := e.eng.RecordHeldFinding(context.Background(), uday, p.operation, "PO changed in Odoo by an administrator", "odoo-audit-log"); err != nil {
				t.Fatalf("finding: %v", err)
			}
			if r := a.check(); len(r.Flagged) != 0 {
				t.Fatalf("flagged twice: %+v", r)
			}
			if c.name == "cancelled in Odoo" {
				a.s.DeleteOrder(p.operation)
				a.check()
				if e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = $1 AND flag = 'ORDER_DELETED_AT_DESTINATION'`, p.operation) != 1 {
					t.Fatal("a later deletion did not replace the change flag")
				}
			}
			requireBuckets(t, "still no refund", e.buckets(p.mandate), r1Committed)
			if !a.writes().Enabled {
				t.Fatal("writes turned off")
			}
		})
	}
}

// R1-DEF-13: the observer's readback flags a material change on its own
// (for example a tax edit that changes the total), without the journal.
func TestR1DEF13ReadbackMismatchFlagsOperation(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	mustDispatch(t, e, p)
	e.settle(p.operation, a.s)
	assertR1Evidence(t, e, p)
	e.eng.Connector = alteredReadback{a.s, func(r *connector.Result) { r.Destination.AmountMinor += 10_800_000 }}
	e.admin(`UPDATE steelmatter.r1_observations SET recorded_at=clock_timestamp()-interval '2 minutes' WHERE operation_id=$1`, p.operation)
	if n, err := e.eng.ObserveR1Due(context.Background()); err != nil || n != 1 {
		t.Fatalf("readback %d %v", n, err)
	}
	e.eng.Connector = a.s
	st, body := e.call("GET", "/v1/dispatches/"+p.operation, "observer-a", nil)
	requireStatus(t, "dispatch", st, body, 200, "")
	if body["flag"] != engine.FlagOrderChangedAtDestination || body["execution"] != engine.DispatchCommitted {
		t.Fatalf("dispatch %v", body)
	}
	requireBuckets(t, "no refund", e.buckets(p.mandate), r1Committed)
	if !a.writes().Enabled {
		t.Fatal("writes turned off")
	}
}
