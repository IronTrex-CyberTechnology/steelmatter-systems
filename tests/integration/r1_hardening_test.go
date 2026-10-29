//go:build integration && failpoints

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

func TestR1RestoredBudgetRefusedBeforeWorkerRuns(t *testing.T) {
	for _, evaluate := range []bool{false, true} {
		t.Run(map[bool]string{true: "evaluate", false: "dispatch"}[evaluate], func(t *testing.T) {
			e := newR1Env(t)
			a := withAnchor(t, e)
			p := e.permitted()
			// Production grace must not create an admission window for a young restore.
			e.eng.Cfg.RestoreGrace = time.Minute
			snap := e.snapshotEngine()
			mustDispatch(t, e, p)
			e.settle(p.operation, a.s)
			e.restoreEngine(snap)
			if evaluate {
				status, body := e.evaluate("agent-a", p.instruction, 1)
				requireStatus(t, "restored evaluation", status, body, 503, "SERVICE_UNAVAILABLE")
			} else {
				if _, err := e.dispatch("agent-a", p); err == nil {
					t.Fatal("restored dispatch admitted before the detector ran")
				}
			}
			if a.writes().Enabled {
				t.Fatal("restore barrier did not disable writes")
			}
			requireBuckets(t, "no new budget movement", e.buckets(p.mandate), r1Reserved)
			if a.s.Creates() != 1 {
				t.Fatal("duplicate order")
			}
		})
	}
}

func TestR1StartupRefusesRestoredHistory(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	p := e.permitted()
	snap := e.snapshotEngine()
	mustDispatch(t, e, p)
	e.settle(p.operation, a.s)
	e.restoreEngine(snap)
	e.eng.Cfg.RestoreGrace = time.Minute
	gate, err := e.eng.InitializeRecovery(context.Background(), a.s)
	if err != nil {
		t.Fatal(err)
	}
	if gate.Passed || a.writes().Enabled {
		t.Fatalf("startup admitted restored database: %+v", gate)
	}
	if _, err := e.dispatch("agent-a", p); err == nil {
		t.Fatal("startup did not fence admission")
	}
}

func TestR1RequiredAnchorCannotBeOmitted(t *testing.T) {
	e := newR1Env(t)
	p := e.permitted()
	e.eng.Cfg.RequireRecoveryAnchor = true
	if _, err := e.dispatch("agent-a", p); err == nil {
		t.Fatal("missing anchor allowed dispatch")
	}
	requireBuckets(t, "missing anchor", e.buckets(p.mandate), r1Reserved)
	if err := e.eng.Ready(context.Background()); err == nil {
		t.Fatal("missing anchor reported ready")
	}
}

func TestR1ConfirmedRepairSurvivesEveryCrashBoundary(t *testing.T) {
	for _, outcome := range []string{engine.RepairCommit, engine.RepairRelease} {
		for _, point := range []string{failpoint.RepairAfterDecision, failpoint.RepairAfterOutcome, failpoint.RepairBeforeCommit} {
			t.Run(outcome+"/"+point, func(t *testing.T) {
				e := newR1Env(t)
				a := withAnchor(t, e)
				p := e.permitted()
				ctx := context.Background()
				mustDispatch(t, e, p)
				if outcome == engine.RepairRelease {
					e.heldNeverSent(p.operation, a.s) // RELEASE needs anchor proof of no create (decision 3)
				} else {
					e.heldVia(p.operation, a.s)
				}
				if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "reviewed outcome", "fixture-evidence"); err != nil {
					t.Fatal(err)
				}
				id, err := e.eng.PrepareRepair(ctx, uday, p.operation, outcome, "fixture-evidence", "reviewed outcome")
				if err != nil {
					t.Fatal(err)
				}
				if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "reviewed"); err != nil {
					t.Fatal(err)
				}
				injected := false
				failpoint.Set(point, func(context.Context) error {
					injected = true
					return failpoint.ErrInjected
				})
				t.Cleanup(failpoint.Clear)
				if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err == nil || !injected {
					t.Fatalf("crash boundary reached=%v: %v", injected, err)
				}
				failpoint.Clear()
				requireBuckets(t, "crashed engine commit", e.buckets(p.mandate), r1Unresolved)
				if a.count(anchor.RepairConfirmed) != 1 {
					t.Fatal("decision was not durable")
				}
				// Recovery works even after fencing, when a fresh human decision is forbidden.
				if _, err := a.c.DisableWrites(ctx, "repair crash"); err != nil {
					t.Fatal(err)
				}
				rep, err := e.eng.ReconcileRestore(ctx, a.s)
				if err != nil || len(rep.Problems) != 0 {
					t.Fatalf("recovery: %+v %v", rep, err)
				}
				want := r1Committed
				status := engine.DispatchCommitted
				if outcome == engine.RepairRelease {
					want = r1Released
					status = engine.DispatchFenced
				}
				requireBuckets(t, "replayed once", e.buckets(p.mandate), want)
				for i := 0; i < 2; i++ {
					if d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err != nil || d.Execution != status {
						t.Fatalf("retry: %+v %v", d, err)
					}
				}
				requireBuckets(t, "retries", e.buckets(p.mandate), want)
				if a.count(anchor.RepairConfirmed) != 1 {
					t.Fatal("decision duplicated")
				}
				if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type='operation.repaired'`); n != 1 {
					t.Fatalf("repair events %d", n)
				}
			})
		}
	}
}
