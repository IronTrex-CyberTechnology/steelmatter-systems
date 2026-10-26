//go:build integration && failpoints

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// Worker failure isolation (SEC-DEF-002, SEC-DEF-003, SEC-DEF-005): one
// persistently failing record is backed off and no longer holds back later
// records, including other tenants'. The failing record is always in
// other-org, which sorts and commits first.

var errStuck = errors.New("injected persistent failure")

// failFor makes the failpoint fail only for requests labelled key.
func failFor(t *testing.T, name, key string) {
	t.Helper()
	failpoint.Set(name, func(ctx context.Context) error {
		if failpoint.KeyFrom(ctx) == key {
			return errStuck
		}
		return nil
	})
	t.Cleanup(failpoint.Clear)
}

// permittedIn is permitted() for any tenant: a fresh mandate owned by owner,
// assigned to actor, and one evaluated 60,000,000 instruction from issuer.
func (e *env) permittedIn(tenantID, owner, issuer, actor, record string) permitted {
	e.t.Helper()
	e.admin(`INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status) VALUES ($1, 'supplier-a', 1, 'ACTIVE') ON CONFLICT DO NOTHING`, tenantID)
	st, body := e.call("POST", "/v1/mandates", owner, map[string]any{
		"actor_ids": []string{actor}, "audience": audience, "supplier_ids": []string{"supplier-a"}, "delivery_ids": []string{"warehouse-a"},
		"currency": "INR", "limit_minor": limit, "expires_at": time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z"),
	})
	requireStatus(e.t, "create mandate", st, body, 201, "")
	m := body["mandate"].(map[string]any)["mandate_id"].(string)
	e.admin(`INSERT INTO steelmatter.workflow_assignments (assignment_id, tenant_id, source_system, mandate_id, actor_id, enabled)
		VALUES ($1, $2, $3, $4, $5, true)`, fmt.Sprintf("asg-%s-%s", m, actor), tenantID, source, m, actor)
	st, inst := e.call("POST", "/v1/instructions", issuer, instructionBody(record, m))
	requireStatus(e.t, "create instruction", st, inst, 201, "")
	id := inst["instruction_id"].(string)
	st, body = e.evaluate(actor, id, 1)
	requireStatus(e.t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	return permitted{mandate: m, instruction: id, action: body["action"].(map[string]any),
		jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
}

// twoTenantsCommitted commits one order in other-org, then one in
// synthetic-org, and clears both execution signatures and observations so
// both are owed by the evidence and observer workers.
func (e *env) twoTenantsCommitted() (stuck, healthy permitted) {
	e.t.Helper()
	stuck = e.permittedIn("other-org", "owner-x", "issuer-x", "agent-x", "po-other-001")
	st, body := e.admit("agent-x", stuck.action, stuck.jws)
	requireStatus(e.t, "admit other-org", st, body, 201, "")
	healthy = e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-001")
	st, body = e.admit("agent-a", healthy.action, healthy.jws)
	requireStatus(e.t, "admit synthetic-org", st, body, 201, "")
	e.adminBypass(`UPDATE steelmatter.operations SET execution_jws = NULL`)
	e.admin(`DELETE FROM steelmatter.observations`)
	return stuck, healthy
}

func (e *env) backoffAttempts(worker, record string) int {
	e.t.Helper()
	return e.count(`SELECT COALESCE(max(attempts), 0) FROM steelmatter.worker_backoff WHERE worker = $1 AND record_id = $2`, worker, record)
}

func (e *env) unresolvedBackoff() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM steelmatter.worker_backoff WHERE resolved_at IS NULL`)
}

// retryNow makes every backed-off record due again.
func (e *env) retryNow() {
	e.t.Helper()
	e.admin(`UPDATE steelmatter.worker_backoff SET next_attempt_at = clock_timestamp() - interval '1 second'`)
}

// runWorker checks one pass of a worker: how many records it completed and
// whether it reported a failure.
func runWorker(t *testing.T, what string, run func(context.Context) (int, error), wantN int, wantErr bool) {
	t.Helper()
	n, err := run(context.Background())
	if n != wantN || (err != nil) != wantErr {
		t.Fatalf("%s: %d (%v); want %d, error %v", what, n, err, wantN, wantErr)
	}
}

func TestSecDef002EvidenceWorkerIsolatesFailingRecord(t *testing.T) {
	e := newEnv(t)
	stuck, healthy := e.twoTenantsCommitted()
	failFor(t, failpoint.EvidenceBeforeSign, stuck.operation)
	sign := e.eng.SignPendingExecutions

	runWorker(t, "first pass", sign, 1, true)
	if e.count(`SELECT count(*) FROM steelmatter.operations WHERE operation_id = $1 AND execution_jws IS NOT NULL`, healthy.operation) != 1 {
		t.Fatal("a failing record in another tenant still blocks evidence signing")
	}
	if a := e.backoffAttempts("evidence", stuck.operation); a != 1 {
		t.Fatalf("attempts %d", a)
	}
	runWorker(t, "backed off", sign, 0, false) // skipped until due, not retried every pass
	e.retryNow()
	runWorker(t, "still failing", sign, 0, true)
	if a := e.backoffAttempts("evidence", stuck.operation); a != 2 {
		t.Fatalf("attempts %d", a)
	}

	failpoint.Clear()
	e.retryNow()
	runWorker(t, "recovered", sign, 1, false)
	if e.count(`SELECT count(*) FROM steelmatter.operations WHERE execution_jws IS NULL`) != 0 || e.unresolvedBackoff() != 0 {
		t.Fatal("recovered record not signed or its backoff not resolved")
	}
}

func TestSecDef003ObserverWorkerIsolatesFailingRecord(t *testing.T) {
	e := newEnv(t)
	stuck, healthy := e.twoTenantsCommitted()
	failFor(t, failpoint.ObserverBeforeReadback, stuck.operation)
	observe := e.eng.ObserveUnobserved

	runWorker(t, "first pass", observe, 1, true)
	if e.count(`SELECT count(*) FROM steelmatter.observations WHERE operation_id = $1`, healthy.operation) != 1 {
		t.Fatal("a failing operation in another tenant still blocks observation")
	}
	if a := e.backoffAttempts("observer", stuck.operation); a != 1 {
		t.Fatalf("attempts %d", a)
	}
	runWorker(t, "backed off", observe, 0, false)

	failpoint.Clear()
	e.retryNow()
	runWorker(t, "recovered", observe, 1, false)
	if e.count(`SELECT count(*) FROM steelmatter.observations`) != 2 || e.unresolvedBackoff() != 0 {
		t.Fatal("recovered operation not observed or its backoff not resolved")
	}
}

func TestSecDef005CleanupWorkerIsolatesFailingRecord(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: time.Second})
	stuck := e.permittedIn("other-org", "owner-x", "issuer-x", "agent-x", "po-other-001")
	healthy := e.permittedIn(tenant, "owner-a", "issuer-a", "agent-a", "po-request-001")
	e.waitExpired(stuck.permitID)
	e.waitExpired(healthy.permitID)
	failFor(t, failpoint.CleanupAfterLocks, stuck.permitID)
	cleanup := e.eng.CleanupExpired

	runWorker(t, "first pass", cleanup, 1, true)
	requireLedger(t, e.ledger(healthy.mandate), ledger{Available: limit})
	if status, _ := e.issuanceStatus(stuck.permitID); status != "ACTIVE" {
		t.Fatalf("failing reservation %s; want ACTIVE (rolled back)", status)
	}
	if a := e.backoffAttempts("cleanup", stuck.permitID); a != 1 {
		t.Fatalf("attempts %d", a)
	}
	runWorker(t, "backed off", cleanup, 0, false)

	failpoint.Clear()
	e.retryNow()
	runWorker(t, "recovered", cleanup, 1, false)
	requireLedger(t, e.ledger(stuck.mandate), ledger{Available: limit})
	if e.unresolvedBackoff() != 0 {
		t.Fatal("backoff not resolved")
	}
}

// A retired key affects every record: the worker refuses before the loop
// and backs nothing off, so signing resumes at once when a key is current.
func TestRetiredKeyIsRefusedWithoutBackoff(t *testing.T) {
	e := newEnv(t)
	e.twoTenantsCommitted()
	if err := e.eng.Trust.Retire("steelmatter-service", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := e.eng.SignPendingExecutions(context.Background()); n != 0 || err == nil {
		t.Fatalf("signed %d (%v) with a retired key", n, err)
	}
	if e.unresolvedBackoff() != 0 {
		t.Fatal("a global refusal was backed off per record")
	}
}

func TestWorkerBacklogReportsOldestOwedRecord(t *testing.T) {
	e := newEnv(t)
	stuck, _ := e.twoTenantsCommitted()
	e.admin(`UPDATE steelmatter.operations SET committed_at = clock_timestamp() - interval '10 minutes' WHERE operation_id = $1`, stuck.operation)
	b, err := e.eng.WorkerBacklog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Evidence <= engine.BacklogAlertAge || b.Observer <= engine.BacklogAlertAge || b.Cleanup != 0 {
		t.Fatalf("backlog %+v", b)
	}
	if _, err := e.eng.SignPendingExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.ObserveUnobserved(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err = e.eng.WorkerBacklog(context.Background()); err != nil || b != (engine.Backlog{}) {
		t.Fatalf("backlog after catching up %+v (%v)", b, err)
	}
}
