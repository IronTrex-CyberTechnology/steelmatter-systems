//go:build integration && failpoints

package integration

import (
	"context"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// Recovery after issuer changes and legitimate backups (NEW-RT0EE-SEC-01,
// NEW-RT0EE-SEC-02, on 0ee221a). The anchored, authorised action is rebuilt;
// conflicting state that never spent is superseded with its reservation
// released, the order is confirmed by lookup (never created again), and the
// gate resumes writes.

const fiveLakh = int64(50_000_000)

func (e *env) amendTo(instructionID string, expected int64, id string) map[string]any {
	e.t.Helper()
	st, body := e.call("POST", "/v1/instructions/"+instructionID+"/revisions", "issuer-a", amendBody(id, expected, line("item-a", 2, 25_000_000)))
	requireStatus(e.t, "amend", st, body, 201, "")
	return body
}

func requireRecovered(t *testing.T, e *env, a *anchored, op string, creates int) {
	t.Helper()
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	rec := a.reconcile()
	if len(rec.Problems) != 0 || len(rec.Recreated) != 1 || rec.Recreated[0] != op {
		t.Fatalf("reconcile %+v", rec)
	}
	d, err := e.eng.GetDispatch(context.Background(), tenant, op)
	if err != nil {
		t.Fatal(err)
	}
	requireExecution(t, "rebuilt", d, engine.DispatchCommitted)
	if g := a.gate(); !g.Passed || !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	if a.s.Creates() != creates {
		t.Fatalf("creates %d, want %d", a.s.Creates(), creates)
	}
}

// SEC-01 prevention: while writes are off, instructions cannot be created or
// amended (503, retryable); after the gate they can.
func TestRT0EESEC01NoIssuerChangesWhileWritesAreOff(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{limit: 2 * limit})
	e.assign(m, "agent-a")
	open := e.instruction("po-open-001", m)
	snap := e.snapshotEngine()
	p := e.permittedFor(m, "po-request-001")
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m))
	requireStatus(t, "re-create while writes are off", st, body, 503, "SERVICE_UNAVAILABLE")
	st, body = e.call("POST", "/v1/instructions/"+open["instruction_id"].(string)+"/revisions", "issuer-a",
		amendBody("amd-1", 1, line("item-a", 2, 25_000_000)))
	requireStatus(t, "amend while writes are off", st, body, 503, "SERVICE_UNAVAILABLE")
	rec := a.reconcile()
	if len(rec.Problems) != 0 {
		t.Fatalf("reconcile %+v", rec)
	}
	if g := a.gate(); !g.Resumed {
		t.Fatalf("gate %+v", g)
	}
	e.amendTo(open["instruction_id"].(string), 1, "amd-1")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m))
	if st == 201 {
		t.Fatalf("source record of the rebuilt instruction reused: %v", body)
	}
}

// SEC-01 residual: the issuer re-created the lost instruction before the
// restore was detected. Recovery supersedes the new one (it never spent)
// and rebuilds the anchored one.
func TestRT0EESEC01RecreatedInstructionIsSuperseded(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	snap := e.snapshotEngine()
	p := e.permittedFor(m, "po-request-001")
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	again := e.instruction("po-request-001", m) // before the check: writes still on
	if again["instruction_id"] == p.instruction {
		t.Fatal("same instruction id")
	}
	requireRecovered(t, e, a, p.operation, 1)
	requireBuckets(t, "recovered", e.buckets(m), r1Committed)
	var by string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT COALESCE(superseded_by, '') FROM steelmatter.instructions WHERE instruction_id = $1`,
		again["instruction_id"]).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if by != p.instruction {
		t.Fatalf("superseded_by %q", by)
	}
	st, body := e.call("POST", "/v1/instructions/"+again["instruction_id"].(string)+"/revisions", "issuer-a",
		amendBody("amd-1", 1, line("item-a", 2, 25_000_000)))
	requireStatus(t, "amend superseded", st, body, 409, "REVISION_STALE")
	if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'instruction.superseded_by_recovery'`); n != 1 {
		t.Fatalf("superseded events %d", n)
	}
}

// SEC-01 residual: the issuer amended the restored instruction before the
// restore was detected. The anchored revision is live again; the amendment
// stays as superseded history.
func TestRT0EESEC01AmendedInstructionIsRestored(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	id := inst["instruction_id"].(string)
	snap := e.snapshotEngine() // instruction exists, permit does not
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	p := permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	e.amendTo(id, 1, "amd-after-restore") // before the check
	requireRecovered(t, e, a, p.operation, 1)
	requireBuckets(t, "recovered", e.buckets(m), r1Committed)
	if n := e.count(`SELECT count(*) FROM steelmatter.action_revisions WHERE instruction_id = '` + id + `' AND action_revision = 2 AND status = 'SUPERSEDED'`); n != 1 {
		t.Fatal("amended revision not kept as superseded history")
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE instruction_id = '` + id + `' AND current_revision = 1`); n != 1 {
		t.Fatal("current revision is not the anchored one")
	}
}

// SEC-02: a legitimate backup taken between creation and amendment (with an
// open reservation for revision 1). Recovery releases that reservation,
// rebuilds the anchored revision 2 and commits it.
func TestRT0EESEC02BackupBeforeAmendmentIsReconciled(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	id := inst["instruction_id"].(string)
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate revision 1", st, body, 201, "")
	snap := e.snapshotEngine() // revision 1 current with 6 lakh reserved
	e.amendTo(id, 1, "amd-1")
	st, body = e.evaluate("agent-a", id, 2)
	requireStatus(t, "evaluate revision 2", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	p := permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	requireRecovered(t, e, a, p.operation, 1)
	requireBuckets(t, "recovered", e.buckets(m), buckets{Available: limit - fiveLakh, Committed: fiveLakh})
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE instruction_id = '` + id + `' AND action_revision = 1 AND status = 'RELEASED' AND release_reason = 'RECOVERED_UNUSED'`); n != 1 {
		t.Fatal("revision 1 reservation not released")
	}
}
