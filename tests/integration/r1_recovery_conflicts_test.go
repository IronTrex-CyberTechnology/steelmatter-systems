//go:build integration && failpoints

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
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

// legacy runs fn as a process that does not consult the recovery anchor, as an
// older build or a stale instance would: it can create, amend, evaluate and
// dispatch while the anchor knows about dispatches the engine database lost.
// Current code refuses those at admission (guardSourceHistory); the recovery
// paths below must still be correct for state that got in this way.
func legacy(e *env, a *anchored, fn func()) {
	e.t.Helper()
	e.eng.Anchor = nil
	defer func() { e.eng.Anchor = a.c }()
	fn()
}

// anchorLegacyIntent records in the anchor the DISPATCH_INTENT that an older
// build would have written for a dispatch made through legacy.
func anchorLegacyIntent(t *testing.T, e *env, a *anchored, p permitted) {
	t.Helper()
	var ir anchor.IntentRecord
	var canonical []byte
	err := e.h.Admin.QueryRow(context.Background(), `SELECT d.tenant_id, d.op_ref, d.instruction_id, d.action_revision, d.mandate_id, d.company_ref,
		d.permit_id, d.control_epoch, d.action_canonical, d.action_digest, d.permit_digest, d.amount_minor, i.source_system, i.source_record_id,
		i.issuer_principal, i.creation_request_digest FROM steelmatter.dispatches d
		JOIN steelmatter.instructions i ON i.tenant_id = d.tenant_id AND i.instruction_id = d.instruction_id WHERE d.op_ref = $1`, p.operation).
		Scan(&ir.Tenant, &ir.OpRef, &ir.InstructionID, &ir.ActionRevision, &ir.MandateID, &ir.CompanyRef, &ir.PermitID, &ir.ControlEpoch,
			&canonical, &ir.ActionDigest, &ir.PermitDigest, &ir.AmountMinor, &ir.SourceSystem, &ir.SourceRecordID, &ir.IssuerPrincipal, &ir.CreationDigest)
	if err != nil {
		t.Fatal(err)
	}
	ir.ActionCanonical, ir.PermitJWS, ir.Currency = string(canonical), p.jws, "INR"
	if _, err := a.c.Append(context.Background(), anchor.DispatchIntent, tenant, p.operation, ir, 0); err != nil {
		t.Fatal(err)
	}
}

// dispatchedThenRestored: the purchase po-request-001 is dispatched and
// committed on mandate m, then the engine database is restored to before it.
func dispatchedThenRestored(t *testing.T) (*env, *anchored, string, permitted) {
	t.Helper()
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	snap := e.snapshotEngine()
	p := e.permittedFor(m, "po-request-001")
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	return e, a, m, p
}

func requireRestoreSuspected(t *testing.T, what string, st int, body map[string]any, a *anchored) {
	t.Helper()
	requireStatus(t, what, st, body, 503, "SERVICE_UNAVAILABLE")
	if !strings.Contains(fmt.Sprint(body["message"]), "RESTORE_SUSPECTED") {
		t.Fatalf("%s: %v", what, body)
	}
	if ws := a.writes(); ws.Enabled || !strings.Contains(ws.Reason, "admission recovery barrier") {
		t.Fatalf("%s: writes %+v", what, ws)
	}
}

func requireRecovered(t *testing.T, e *env, a *anchored, op string, creates int) {
	t.Helper()
	a.check() // turns writes off if the restore is not yet known
	if ws := a.writes(); ws.Enabled {
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
	var again map[string]any
	legacy(e, a, func() { again = e.instruction("po-request-001", m) }) // before the check, by a process that does not consult the anchor
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
	// Recovery grants nothing new: neither instruction can be evaluated
	// again, and no further order is created.
	for _, id := range []string{again["instruction_id"].(string), p.instruction} {
		if st, body := e.evaluate("agent-a", id, 1); st == 201 {
			t.Fatalf("permit issued after recovery for %s: %v", id, body)
		}
	}
	if a.s.Creates() != 1 {
		t.Fatalf("creates %d", a.s.Creates())
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
	legacy(e, a, func() { e.amendTo(id, 1, "amd-after-restore") }) // before the check, by a process that does not consult the anchor
	requireRecovered(t, e, a, p.operation, 1)
	requireBuckets(t, "recovered", e.buckets(m), r1Committed)
	if n := e.count(`SELECT count(*) FROM steelmatter.action_revisions WHERE instruction_id = '` + id + `' AND action_revision = 2 AND status = 'SUPERSEDED'`); n != 1 {
		t.Fatal("amended revision not kept as superseded history")
	}
	// NEW-RTFC79 API finding: retrying the amendment that recovery superseded
	// is no longer a 200; it says so and names the live revision.
	st, body = e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", amendBody("amd-after-restore", 1, line("item-a", 2, 25_000_000)))
	requireStatus(t, "retry of a superseded amendment", st, body, 409, "REVISION_STALE")
	if !strings.Contains(fmt.Sprint(body["message"]), "AMENDMENT_SUPERSEDED") {
		t.Fatalf("retry: %v", body)
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

// Operator review (Aakhil, 30 Oct 2026): an issuer change that was
// dispatched, spent or has an uncertain outcome is never superseded.
// Recovery stops with CONFLICT_SPENT, keeps the gate closed, leaves the
// conflicting operation and its amount as they are and sends nothing. The
// operator then adopts the anchored operation (NEW-RTFC79-01): both amounts
// stay held or committed, nothing is created or released, and the gate passes
// only with the operator's finding on every operation of the source record.
func TestRT0EESEC01DispatchedConflictStopsForReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle bool
	}{{"committed", true}, {"outcome uncertain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e, a, m, p := dispatchedThenRestored(t)
			// A process that does not consult the anchor re-creates the source
			// record on another budget and dispatches it (current code
			// refuses this: TestRTFC7901...).
			m2 := e.mandate(mandateOpts{})
			e.assign(m2, "agent-a")
			var q permitted
			legacy(e, a, func() {
				q = e.permittedFor(m2, "po-request-001")
				mustDispatch(t, e, q)
			})
			anchorLegacyIntent(t, e, a, q)
			if tc.settle {
				requireExecution(t, "conflict settle", e.settle(q.operation, a.s), engine.DispatchCommitted)
			}
			// Resolution is for recovery only.
			st, body := e.call("POST", "/v1/recovery/conflicts/"+p.operation+"/adopt", "operator-a", map[string]any{"evidence_ref": "TICKET-1", "note": "reviewed"})
			requireStatus(t, "adopt while writes are on", st, body, 403, "FORBIDDEN")
			creates := a.s.Creates()
			if r := a.check(); !r.WritesDisabled {
				t.Fatal("restore not detected")
			}
			rec := a.reconcile()
			found := false
			for _, pr := range rec.Problems {
				found = found || strings.Contains(pr, "CONFLICT_SPENT")
			}
			if !found || len(rec.Recreated) != 0 {
				t.Fatalf("reconcile %+v", rec)
			}
			if g := a.gate(); g.Passed {
				t.Fatalf("gate passed: %+v", g)
			}
			if tc.settle {
				requireBuckets(t, "conflict budget", e.buckets(m2), r1Committed)
			} else {
				// Never sent: either still held as unresolved, or proven
				// never created by lookup and fence (BLOCK) and released.
				d, err := e.eng.GetDispatch(context.Background(), tenant, q.operation)
				if err != nil {
					t.Fatal(err)
				}
				b := e.buckets(m2)
				held := d.Execution == engine.DispatchUnresolved || d.Execution == engine.DispatchPending
				if !(held && b == r1Unresolved) && !(d.Execution == engine.DispatchFenced && b == r1Released) {
					t.Fatalf("conflict %s, budget %+v", d.Execution, b)
				}
			}
			if a.s.Creates() != creates {
				t.Fatalf("recovery sent a create: %d -> %d", creates, a.s.Creates())
			}
			if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE superseded_by IS NOT NULL`); n != 0 {
				t.Fatal("a dispatched instruction was superseded")
			}
			if n := e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = '` + p.operation + `'`); n != 0 {
				t.Fatal("the anchored operation was rebuilt over the conflict")
			}

			// Operator procedure. Only an operator, with evidence.
			path := "/v1/recovery/conflicts/" + p.operation + "/adopt"
			st, body = e.call("POST", path, "issuer-a", map[string]any{"evidence_ref": "TICKET-1", "note": "reviewed"})
			requireStatus(t, "issuer adopts", st, body, 403, "FORBIDDEN")
			st, body = e.call("POST", path, "operator-a", map[string]any{"evidence_ref": "TICKET-1"})
			requireStatus(t, "adopt without a note", st, body, 400, "")
			st, body = e.call("POST", path, "operator-a", map[string]any{"evidence_ref": "TICKET-1", "note": "both orders reviewed in Odoo; the duplicate is cancelled by purchasing"})
			requireStatus(t, "adopt", st, body, 200, "")
			if body["rebuilt"] != true || body["superseded_instruction"] != q.instruction || body["instruction_id"] != p.instruction {
				t.Fatalf("adopt %v", body)
			}
			if a.s.Creates() != creates {
				t.Fatalf("adopt sent a create: %d -> %d", creates, a.s.Creates())
			}
			st, body = e.call("POST", path, "operator-a", map[string]any{"evidence_ref": "TICKET-1", "note": "again"})
			requireStatus(t, "adopt again", st, body, 200, "")
			if body["rebuilt"] == true || len(body["finding_seqs"].([]any)) != 0 {
				t.Fatalf("a repeat rebuilt or recorded twice: %v", body)
			}
			// Rows kept; the anchored instruction holds the source key.
			if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE instruction_id = '` + q.instruction + `' AND superseded_by = '` + p.instruction + `'`); n != 1 {
				t.Fatal("conflicting instruction not handed over")
			}
			if n := e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = '` + q.operation + `'`); n != 1 {
				t.Fatal("the spent instruction's operation was removed")
			}
			rec = a.reconcile()
			if len(rec.Problems) != 0 {
				t.Fatalf("reconcile after adopt %+v", rec)
			}
			d, err := e.eng.GetDispatch(context.Background(), tenant, p.operation)
			if err != nil {
				t.Fatal(err)
			}
			requireExecution(t, "adopted operation", d, engine.DispatchCommitted)
			requireBuckets(t, "anchored budget", e.buckets(m), r1Committed)
			if g := a.gate(); !g.Passed || !g.Resumed {
				t.Fatalf("gate after adopt: %+v", g)
			}
			if a.s.Creates() != creates {
				t.Fatalf("recovery sent a create: %d -> %d", creates, a.s.Creates())
			}
			a.verify()
		})
	}
}

// NEW-RTFC79-01 (the window): between a restore and its detection the engine
// has no record of the source record, and Odoo only deduplicates one op_ref.
// The anchor shows the dispatch, so create, evaluate and dispatch for the same
// source record under ANOTHER mandate or instruction are refused, writes are
// turned off, no second order is created, and recovery rebuilds the original.
func TestRTFC7901NoSecondPurchaseForTheSameSourceRecord(t *testing.T) {
	t.Run("create under another mandate", func(t *testing.T) {
		e, a, _, p := dispatchedThenRestored(t)
		m2 := e.mandate(mandateOpts{})
		e.assign(m2, "agent-a")
		st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m2))
		requireRestoreSuspected(t, "re-create", st, body, a)
		if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE source_record_id = 'po-request-001'`); n != 0 {
			t.Fatal("an instruction was created")
		}
		requireRecovered(t, e, a, p.operation, 1)
		if n := e.count(`SELECT count(*) FROM steelmatter.instructions WHERE superseded_by IS NOT NULL`); n != 0 {
			t.Fatal("something was superseded")
		}
		st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m2))
		if st == 201 {
			t.Fatalf("source record reused after recovery: %v", body)
		}
	})
	t.Run("evaluate", func(t *testing.T) {
		e, a, _, p := dispatchedThenRestored(t)
		m2 := e.mandate(mandateOpts{})
		e.assign(m2, "agent-a")
		var inst map[string]any
		legacy(e, a, func() { inst = e.instruction("po-request-001", m2) })
		st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
		requireRestoreSuspected(t, "evaluate", st, body, a)
		if n := e.count(`SELECT count(*) FROM steelmatter.issuances WHERE instruction_id = '` + inst["instruction_id"].(string) + `'`); n != 0 {
			t.Fatal("a permit was issued")
		}
		requireRecovered(t, e, a, p.operation, 1) // the unspent instruction is superseded
	})
	t.Run("dispatch", func(t *testing.T) {
		e, a, m2, p := dispatchedThenRestored(t)
		_ = m2
		m3 := e.mandate(mandateOpts{})
		e.assign(m3, "agent-a")
		var q permitted
		legacy(e, a, func() { q = e.permittedFor(m3, "po-request-001") }) // a permit exists; nothing is sent yet
		_, err := e.dispatch("agent-a", q)
		if err == nil || !strings.Contains(err.Error(), "RESTORE_SUSPECTED") {
			t.Fatalf("dispatch: %v", err)
		}
		if ws := a.writes(); ws.Enabled {
			t.Fatal("writes still on")
		}
		if s, _ := e.issuanceStatus(q.permitID); s != "ACTIVE" {
			t.Fatalf("permit %s: it was consumed", s)
		}
		if n := e.count(`SELECT count(*) FROM steelmatter.dispatches WHERE op_ref = '` + q.operation + `'`); n != 0 {
			t.Fatal("a dispatch was recorded")
		}
		if a.s.Creates() != 1 {
			t.Fatalf("creates %d: a second purchase was sent", a.s.Creates())
		}
		requireRecovered(t, e, a, p.operation, 1)
		requireBuckets(t, "second budget", e.buckets(m3), buckets{Available: limit})
	})
}

// NEW-RTFC79-02 (the window): an amendment after a restore that reuses an
// anchored revision number with a different action is refused before the
// number is used; recovery restores the anchored revision untouched.
func TestRTFC7902AmendCannotReuseAnchoredRevisionNumber(t *testing.T) {
	e := newR1Env(t)
	a := withAnchor(t, e)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	id := inst["instruction_id"].(string)
	snap := e.snapshotEngine() // revision 1 only
	e.amendTo(id, 1, "amd-1")
	st, body := e.evaluate("agent-a", id, 2)
	requireStatus(t, "evaluate revision 2", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	p := permitted{mandate: m, instruction: id, action: body["action"].(map[string]any), jws: body["permit_jws"].(string),
		permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
	var anchoredDigest string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT action_digest FROM steelmatter.action_revisions WHERE instruction_id = $1 AND action_revision = 2`, id).Scan(&anchoredDigest); err != nil {
		t.Fatal(err)
	}
	mustDispatch(t, e, p)
	requireExecution(t, "settle", e.settle(p.operation, a.s), engine.DispatchCommitted)
	e.restoreEngine(snap)
	st, body = e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", amendBody("amd-other", 1, line("item-a", 1, 10_000_000)))
	requireRestoreSuspected(t, "amend with a different action", st, body, a)
	if n := e.count(`SELECT count(*) FROM steelmatter.action_revisions WHERE instruction_id = '` + id + `' AND action_revision = 2`); n != 0 {
		t.Fatal("revision 2 was used by the issuer")
	}
	requireRecovered(t, e, a, p.operation, 1)
	var got, status string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT action_digest, status FROM steelmatter.action_revisions WHERE instruction_id = $1 AND action_revision = 2`, id).Scan(&got, &status); err != nil {
		t.Fatal(err)
	}
	if got != anchoredDigest || status != "COMMITTED" {
		t.Fatalf("revision 2 is %s %s, want the anchored %s COMMITTED", got, status, anchoredDigest)
	}
}

// NEW-RTFC79 API finding (a): who is allowed is decided before recovery
// state, so a caller who is not an entitled issuer gets 403 (404 for an
// instruction outside its source systems), never 503.
func TestRTFC79RoleChecksComeBeforeRecoveryState(t *testing.T) {
	e, a, m, p := dispatchedThenRestored(t)
	open := e.instruction("po-open-002", m)
	if r := a.check(); !r.WritesDisabled {
		t.Fatal("restore not detected")
	}
	for _, who := range []string{"agent-a", "operator-a", "observer-a", "owner-a", "issuer-b"} {
		st, body := e.call("POST", "/v1/instructions", who, instructionBody("po-request-001", m))
		requireStatus(t, who+" creates during recovery", st, body, 403, "FORBIDDEN")
		st, body = e.call("POST", "/v1/instructions/"+open["instruction_id"].(string)+"/revisions", who, amendBody("amd-r", 1, line("item-a", 2, 25_000_000)))
		want := 403
		if who == "issuer-b" {
			want = 404 // an issuer outside the source system: no existence oracle
		}
		requireStatus(t, who+" amends during recovery", st, body, want, "")
	}
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-request-001", m))
	requireStatus(t, "the entitled issuer during recovery", st, body, 503, "SERVICE_UNAVAILABLE")
	st, body = e.call("POST", "/v1/instructions/"+open["instruction_id"].(string)+"/revisions", "issuer-a", amendBody("amd-r", 1, line("item-a", 2, 25_000_000)))
	requireStatus(t, "the entitled issuer amends during recovery", st, body, 503, "SERVICE_UNAVAILABLE")
	_ = p
}

// NEW-RTFC79 API findings (b) and (c).
func TestRTFC79AmendmentReplayAndInstructionStatus(t *testing.T) {
	t.Run("replay marks an amendment a later one replaced", func(t *testing.T) {
		e := newR1Env(t)
		m := e.mandate(mandateOpts{})
		e.assign(m, "agent-a")
		id := e.instruction("po-request-001", m)["instruction_id"].(string)
		first := e.amendTo(id, 1, "amd-1")
		if first["status"] != "CURRENT" || first["effective"] != true || fmt.Sprint(first["current_action_revision"]) != "2" {
			t.Fatalf("new amendment %v", first)
		}
		st, body := e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", amendBody("amd-2", 2, line("item-a", 2, 20_000_000)))
		requireStatus(t, "second amendment", st, body, 201, "")
		st, body = e.call("POST", "/v1/instructions/"+id+"/revisions", "issuer-a", amendBody("amd-1", 1, line("item-a", 2, 25_000_000)))
		requireStatus(t, "replay of the first", st, body, 200, "")
		if body["status"] != "SUPERSEDED" || body["effective"] != false || fmt.Sprint(body["current_action_revision"]) != "3" {
			t.Fatalf("replay %v", body)
		}
	})
	t.Run("recovery supersedes an instruction and the API says so", func(t *testing.T) {
		e, a, m, p := dispatchedThenRestored(t)
		var again map[string]any
		legacy(e, a, func() { again = e.instruction("po-request-001", m) })
		requireRecovered(t, e, a, p.operation, 1)
		id := again["instruction_id"].(string)
		st, body := e.call("GET", "/v1/instructions/"+id, "issuer-a", nil)
		requireStatus(t, "get superseded", st, body, 200, "")
		if body["status"] != "SUPERSEDED" || body["superseded_by"] != p.instruction {
			t.Fatalf("superseded instruction %v", body)
		}
		st, body = e.call("GET", "/v1/instructions/"+p.instruction, "agent-a", nil)
		requireStatus(t, "get anchored as the actor", st, body, 200, "")
		if body["status"] != "COMMITTED" || body["superseded_by"] != nil {
			t.Fatalf("anchored instruction %v", body)
		}
		for _, who := range []string{"issuer-b", "agent-b"} {
			st, body = e.call("GET", "/v1/instructions/"+id, who, nil)
			requireStatus(t, who+" reads", st, body, 404, "NOT_FOUND")
		}
		for _, who := range []string{"observer-a", "operator-a", "owner-a"} {
			st, body = e.call("GET", "/v1/instructions/"+id, who, nil)
			requireStatus(t, who+" reads", st, body, 403, "FORBIDDEN")
		}
		// Neither instruction can spend again.
		st, body = e.evaluate("agent-a", id, 1)
		requireStatus(t, "evaluate superseded", st, body, 409, "REVISION_STALE")
	})
}

var _ = json.Marshal
