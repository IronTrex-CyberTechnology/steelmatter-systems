//go:build odoo

// Tests against the local Odoo sandbox (destinations/odoo/sandbox/up). Run
// with destinations/odoo/sandbox/test-connector, which sets:
//
//	STEELMATTER_ODOO_URL, STEELMATTER_ODOO_DB,
//	STEELMATTER_ODOO_EXECUTOR_PASSWORD, STEELMATTER_ODOO_OBSERVER_PASSWORD
//
// The same scenarios run through Uday's stub connector and the real Odoo
// connector, and must give the same outcomes and proof flags: the engine can
// swap one for the other.
package odoo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/odoo"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector/stub"
)

const supplier = "sup-chairs"

func sandbox(t *testing.T) *odoo.Connector {
	t.Helper()
	url := os.Getenv("STEELMATTER_ODOO_URL")
	if url == "" {
		t.Skip("STEELMATTER_ODOO_URL is not set; run destinations/odoo/sandbox/test-connector")
	}
	c, err := odoo.New(odoo.Config{
		URL: url, Database: os.Getenv("STEELMATTER_ODOO_DB"), Instance: "sandbox", Timeout: 30 * time.Second,
		Executor: odoo.Credentials{Login: "sm_executor", Password: os.Getenv("STEELMATTER_ODOO_EXECUTOR_PASSWORD")},
		Observer: odoo.Credentials{Login: "sm_observer", Password: os.Getenv("STEELMATTER_ODOO_OBSERVER_PASSWORD")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var runID = func() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

func opRef(name string) string { return "go-" + runID + "-" + name }

func action(op string, rev int64) connector.Action {
	return connector.Action{OpRef: op, ActionRevision: 1, CompanyRef: "acme-in", DeliveryRef: "wh-chennai", SupplierRef: supplier,
		SupplierRevision: rev, Lines: []connector.Line{{ItemRef: "chair-ergo", Quantity: 10, UnitPriceMinor: 600000}},
		AmountMinor: 6000000, Currency: "INR", PermitDigest: "sha256:" + op}
}

// summary is what must agree between the stub and the real destination.
type summary struct {
	Outcome        connector.Outcome
	ProvesNoOrder  bool
	HeldOverPrior  bool
	HasDestination bool
}

func sum(r connector.Result) summary {
	return summary{r.Outcome, r.ProvesNoOrder(), r.BlockedOverPriorCreate(), r.Destination != nil}
}

type step func(ctx context.Context, c connector.Connector, op string, rev int64) connector.Result

func execute(mod func(*connector.Action)) step {
	return func(ctx context.Context, c connector.Connector, op string, rev int64) connector.Result {
		a := action(op, rev)
		if mod != nil {
			mod(&a)
		}
		r, _ := c.Execute(ctx, a)
		return r
	}
}

func lookup(ctx context.Context, c connector.Connector, op string, _ int64) connector.Result {
	r, _ := c.Lookup(ctx, op)
	return r
}

func fence(ctx context.Context, c connector.Connector, op string, _ int64) connector.Result {
	r, _ := c.Fence(ctx, op, 0)
	return r
}

func TestSameOutcomesAsTheStub(t *testing.T) {
	real := sandbox(t)
	ctx := context.Background()
	rev, err := real.SupplierRevision(ctx, supplier)
	if err != nil {
		t.Fatal(err)
	}
	scenarios := map[string][]step{
		"create, retry, other digest, lookup, fence": {execute(nil), execute(nil),
			execute(func(a *connector.Action) { a.PermitDigest = "sha256:other" }), lookup, fence},
		"fence first, late create, lookup, fence again": {fence, execute(nil), lookup, fence},
		"lookup of an unknown op_ref":                   {lookup},
		"stale supplier":                                {execute(func(a *connector.Action) { a.SupplierRevision = rev + 100 }), lookup},
		"restore gate":                                  {execute(func(a *connector.Action) { a.ExpectedMinSeq = 1 << 40 })},
	}
	for name, steps := range scenarios {
		t.Run(name, func(t *testing.T) {
			s := stub.New()
			s.SetSupplierRevision(supplier, rev)
			op := opRef("parity-" + hex.EncodeToString([]byte(name))[:12])
			for i, st := range steps {
				want, got := st(ctx, s, op, rev), st(ctx, real, op, rev)
				if sum(got) != sum(want) {
					t.Fatalf("step %d: odoo %+v (reason %q), stub %+v", i, sum(got), got.Reason, sum(want))
				}
			}
		})
	}
}

func TestCreatedOrderCarriesTheAuthorisedFacts(t *testing.T) {
	c := sandbox(t)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	a := action(opRef("facts"), rev)
	r, _ := c.Execute(ctx, a)
	if r.Outcome != connector.Created {
		t.Fatalf("got %+v", r)
	}
	o := r.Destination
	if o.State != connector.StateConfirmed || o.CompanyRef != a.CompanyRef || o.DeliveryRef != a.DeliveryRef || o.SupplierRef != a.SupplierRef ||
		o.AmountMinor != a.AmountMinor || o.Currency != a.Currency || o.PermitDigest != a.PermitDigest || !reflect.DeepEqual(o.Lines, a.Lines) {
		t.Fatalf("order facts differ from the action:\n%+v\n%+v", o, a)
	}
	if r.Seq <= 0 || r.Incarnation == "" || r.OdooTxTime.IsZero() || r.ConnectorReceivedAt.IsZero() {
		t.Fatalf("evidence fields missing: %+v", r)
	}
	if ob, _ := c.Observe(ctx, a); ob.Outcome != connector.Match {
		t.Fatalf("observe %+v", ob)
	}
	changed := a
	changed.AmountMinor++
	if ob, _ := c.Observe(ctx, changed); ob.Outcome != connector.Mismatch {
		t.Fatalf("observe of a different action %+v", ob)
	}
}

func TestReadiness(t *testing.T) {
	c := sandbox(t)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	if r, _ := c.Readiness(ctx, action(opRef("ready"), rev)); r.Outcome != connector.Ready {
		t.Fatalf("got %+v", r)
	}
	if r, _ := c.Readiness(ctx, action(opRef("ready"), rev+1)); r.Outcome != connector.NotReady || r.Reason != connector.ReasonSupplierChanged {
		t.Fatalf("got %+v", r)
	}
	missing := action(opRef("ready"), rev)
	missing.Lines[0].ItemRef = "no-such-item"
	if r, _ := c.Readiness(ctx, missing); r.Outcome != connector.NotReady || r.Reason != connector.ReasonMappingMissing {
		t.Fatalf("got %+v", r)
	}
}

func TestCreateAndFenceRaceThroughTheConnector(t *testing.T) {
	c := sandbox(t)
	ctx := context.Background()
	rev, _ := c.SupplierRevision(ctx, supplier)
	seen := map[[2]connector.Outcome]int{}
	for i := 0; i < 20; i++ {
		op := opRef("race-" + hex.EncodeToString([]byte{byte(i)}))
		var ex, fe connector.Result
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; ex, _ = c.Execute(ctx, action(op, rev)) }()
		go func() { defer wg.Done(); <-start; fe, _ = c.Fence(ctx, op, 0) }()
		close(start)
		wg.Wait()
		pair := [2]connector.Outcome{ex.Outcome, fe.Outcome}
		seen[pair]++
		switch pair {
		case [2]connector.Outcome{connector.Created, connector.Exists}:
			if fe.ProvesNoOrder() {
				t.Fatalf("%s: fence proves no order while one exists", op)
			}
		case [2]connector.Outcome{connector.RefusedFenced, connector.Fenced}:
			if !fe.ProvesNoOrder() {
				t.Fatalf("%s: FENCED without proof: %+v", op, fe)
			}
			if lk, _ := c.Lookup(ctx, op); lk.Outcome != connector.Fenced {
				t.Fatalf("%s: lookup after fence %+v", op, lk)
			}
		default:
			t.Fatalf("%s: unexpected pair %v (%q, %q)", op, pair, ex.Reason, fe.Reason)
		}
	}
	t.Logf("orderings seen: %v", seen)
}

func TestListReturnsEveryRowInCommitOrder(t *testing.T) {
	c := sandbox(t)
	ctx := context.Background()
	var cp, head int64
	var rows []connector.ListRow
	for {
		p, err := c.List(ctx, cp, 50)
		if err != nil {
			t.Fatal(err)
		}
		head = p.HeadSeq
		if len(p.Rows) == 0 {
			break
		}
		rows = append(rows, p.Rows...)
		cp = p.NextCheckpoint
	}
	if len(rows) == 0 || rows[len(rows)-1].Seq != head {
		t.Fatalf("listing ends at %d, head is %d", rows[len(rows)-1].Seq, head)
	}
	for i := range rows {
		if rows[i].Seq != int64(i+1) {
			t.Fatalf("row %d has seq %d: the listing skipped or repeated a row", i, rows[i].Seq)
		}
	}
}
