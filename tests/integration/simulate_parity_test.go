//go:build integration && failpoints

package integration

import (
	"context"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// Simulation answers what evaluate would do now (document 03). Its capacity
// answer must account for an issuance evaluate would complete (unsigned,
// in window) or release (expired, unused), and it must create nothing.
func TestSimulateMatchesEvaluateForLiveIssuances(t *testing.T) {
	t.Run("unsigned issuance in window", func(t *testing.T) {
		e := newEnv(t)
		m := e.mandate(mandateOpts{})
		e.assign(m, "agent-a")
		id := e.instruction("po-request-001", m)["instruction_id"].(string)
		failpoint.Set(failpoint.EvaluateAfterReserve, func(context.Context) error { return errDBDropped })
		st, body := e.evaluate("agent-a", id, 1)
		failpoint.Clear()
		requireStatus(t, "interrupted evaluate", st, body, 503, "SERVICE_UNAVAILABLE")
		requireLedger(t, e.ledger(m), ledger{Available: 40_000_000, Reserved: sixLakh})

		before := e.snapshot(m)
		st, body = e.simulate("operator-a", id, 1)
		requireStatus(t, "simulate", st, body, 200, "")
		if body["decision"] != "PERMIT_WOULD_ISSUE" {
			t.Fatalf("simulate %v; evaluate would complete the unsigned issuance", body)
		}
		if e.snapshot(m) != before {
			t.Fatal("simulation changed state")
		}
		st, body = e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
	})

	t.Run("expired unused permit", func(t *testing.T) {
		e := newEnv(t)
		p := e.permitted()
		// Age the issued permit past its expiry (same 5-minute lifetime).
		e.adminBypass(`UPDATE steelmatter.issuances SET issued_at = issued_at - interval '10 minutes', expires_at = expires_at - interval '10 minutes'
			WHERE permit_id = $1`, p.permitID)
		requireLedger(t, e.ledger(p.mandate), ledger{Available: 40_000_000, Reserved: sixLakh})

		before := e.snapshot(p.mandate)
		st, body := e.simulate("operator-a", p.instruction, 1)
		requireStatus(t, "simulate", st, body, 200, "")
		if body["decision"] != "PERMIT_WOULD_ISSUE" {
			t.Fatalf("simulate %v; evaluate would release the expired permit and issue", body)
		}
		if e.snapshot(p.mandate) != before {
			t.Fatal("simulation changed state")
		}
		st, body = e.evaluate("agent-a", p.instruction, 1)
		requireStatus(t, "evaluate", st, body, 201, "")
		requireLedger(t, e.ledger(p.mandate), ledger{Available: 40_000_000, Reserved: sixLakh})
	})
}
