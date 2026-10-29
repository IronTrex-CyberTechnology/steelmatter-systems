//go:build integration && failpoints

package integration

import (
	"context"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// R1-DEF-07: a repair whose decision reached the anchor but whose engine
// commit failed, or whose engine database was restored to before it, is
// replayed by reconciliation and the resume gate then passes.
func TestR1DEF07RepairCommitSplitRecovers(t *testing.T) {
	for _, variant := range []string{"commit-fails", "engine-restored-after-repair"} {
		t.Run(variant, func(t *testing.T) {
			e := newR1Env(t)
			a := withAnchor(t, e)
			p := e.permitted()
			ctx := context.Background()
			mustDispatch(t, e, p)
			e.heldVia(p.operation, a.s)
			if _, err := e.eng.RecordHeldFinding(ctx, uday, p.operation, "held", "ev"); err != nil {
				t.Fatal(err)
			}
			id, err := e.eng.PrepareRepair(ctx, uday, p.operation, engine.RepairCommit, "ev", "s")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.eng.ReviewRepair(ctx, kaif, p.operation, id, true, "ok"); err != nil {
				t.Fatal(err)
			}
			var snap string
			if variant == "commit-fails" {
				failpoint.Set(failpoint.RepairBeforeCommit, func(context.Context) error { return failpoint.ErrInjected })
				if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err == nil {
					t.Fatal("expected the engine commit to fail")
				}
				failpoint.Clear()
			} else {
				snap = e.snapshotEngine()
				if _, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err != nil {
					t.Fatal(err)
				}
				e.restoreEngine(snap)
			}
			if e.dispatchStatus(p.operation) != engine.DispatchHeld {
				t.Fatalf("engine status %s", e.dispatchStatus(p.operation))
			}
			if r := a.check(); !r.WritesDisabled {
				t.Fatalf("the split was not detected: %+v", r)
			}
			{
				rec := a.reconcile()
				if len(rec.Problems) != 0 {
					t.Fatalf("reconcile problems %v", rec.Problems)
				}
			}
			if g := a.gate(); !g.Resumed {
				t.Fatalf("gate did not resume: %+v", g)
			}
			if e.dispatchStatus(p.operation) != engine.DispatchCommitted {
				t.Fatalf("status after reconcile %s", e.dispatchStatus(p.operation))
			}
			if d, err := e.eng.ConfirmRepair(ctx, aakhil, p.operation, id, a.s); err != nil || d.Execution != engine.DispatchCommitted {
				t.Fatalf("retry of the confirmation: %+v %v", d, err)
			}
			requireBuckets(t, "committed once", e.buckets(p.mandate), r1Committed)
			if !a.writes().Enabled {
				t.Fatal("writes stayed off")
			}
		})
	}
}
