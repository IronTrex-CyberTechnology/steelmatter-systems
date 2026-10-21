//go:build integration

package locking

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/dbtest"
)

const (
	limit  int64 = 100_000_000 // INR 10,00,000 in paise
	amount int64 = 60_000_000  // INR 6,00,000
)

// Fixture is one mandate, one instruction at revision 1 and one issuance.
type Fixture struct {
	H           *dbtest.Harness
	Tenant      string
	Mandate     string
	Instruction string
	Permit      string
}

type seedOpts struct {
	issuanceStatus  string        // UNSIGNED or ACTIVE
	permitExpiresIn time.Duration // relative to now; negative = already expired
}

// seed creates the synthetic starting state as the administrator. The
// issuance already holds a reservation of amount, so the ledger starts at
// available 40,000,000 / reserved 60,000,000.
func seed(t *testing.T, o seedOpts) *Fixture {
	t.Helper()
	h := dbtest.New(t)
	f := &Fixture{H: h, Tenant: "synthetic-org", Mandate: "mandate-001", Instruction: "inst-001", Permit: "permit-001"}
	if o.issuanceStatus == "" {
		o.issuanceStatus = "ACTIVE"
	}
	if o.permitExpiresIn == 0 {
		o.permitExpiresIn = 4 * time.Minute // lifetime issuedAgo + 4m stays within the 5-minute window
	}
	issuedAgo := 10 * time.Second
	if o.permitExpiresIn < 0 {
		issuedAgo = -o.permitExpiresIn + time.Minute
	}
	var jws any
	if o.issuanceStatus == "ACTIVE" {
		jws = "prototype.unsigned.placeholder"
	}
	ctx := context.Background()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind) VALUES
			('owner-a', $1, 'OWNER'), ('issuer-a', $1, 'ISSUER'), ('agent-a', $1, 'ACTOR'), ('agent-b', $1, 'ACTOR')`, []any{f.Tenant}},
		{`INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status) VALUES ($1, 'supplier-a', 1, 'ACTIVE')`, []any{f.Tenant}},
		{`INSERT INTO steelmatter.mandates (tenant_id, mandate_id, revision, control_epoch, status, owner_principal, audience,
			actor_ids, supplier_ids, delivery_ids, currency, limit_minor, available_minor, reserved_minor, committed_minor,
			payload_canonical, mandate_digest, issued_at, expires_at)
		  VALUES ($1, $2, 1, 1, 'ACTIVE', 'owner-a', 'synthetic-procurement', '{agent-a,agent-b}', '{supplier-a}', '{warehouse-a}', 'INR',
			$3::bigint, $3::bigint - $4::bigint, $4::bigint, 0, '\x7b7d'::bytea, repeat('1', 64), now() - interval '1 hour', now() + interval '1 hour')`,
			[]any{f.Tenant, f.Mandate, limit, amount}},
		{`INSERT INTO steelmatter.instructions (tenant_id, instruction_id, source_system, source_record_id, mandate_id, actor_id,
			issuer_principal, creation_request_digest, current_revision, created_at)
		  VALUES ($1, $2, 'synthetic-erp', 'po-request-001', $3, 'agent-a', 'issuer-a', repeat('e', 64), 1, now())`,
			[]any{f.Tenant, f.Instruction, f.Mandate}},
		{`INSERT INTO steelmatter.action_revisions (tenant_id, instruction_id, action_revision, status, action_canonical, action_digest,
			supplier_id, supplier_revision, delivery_id, audience, amount_minor, created_at)
		  VALUES ($1, $2, 1, 'CURRENT', '\x7b7d'::bytea, repeat('0', 64), 'supplier-a', 1, 'warehouse-a', 'synthetic-procurement', $3, now())`,
			[]any{f.Tenant, f.Instruction, amount}},
		{`INSERT INTO steelmatter.issuances (tenant_id, permit_id, reservation_id, operation_id, instruction_id, action_revision, mandate_id,
			control_epoch, amount_minor, status, permit_canonical, permit_digest, permit_jws, issued_at, expires_at, signed_at)
		  SELECT $1, $2, 'res-001', 'op-001', $3, 1, $4, 1, $5, $6, '\x7b7d'::bytea, repeat('2', 64), $7::text,
			c.ts - $8::double precision * interval '1 second', c.ts + $9::double precision * interval '1 second',
			CASE WHEN $7::text IS NULL THEN NULL ELSE c.ts END
		  FROM (SELECT clock_timestamp() AS ts) c`,
			[]any{f.Tenant, f.Permit, f.Instruction, f.Mandate, amount, o.issuanceStatus, jws, issuedAgo.Seconds(), o.permitExpiresIn.Seconds()}},
	}
	for _, s := range stmts {
		if _, err := h.Admin.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return f
}

// State is the persisted state every scenario inspects.
type State struct {
	MandateStatus                                 string
	Epoch                                         int64
	Available, Reserved, Committed, Unresolved    int64
	IssuanceStatus, ReleaseReason                 string
	Orders, Obligations, Operations, OutboxEvents int
	CurrentRevision                               int64
}

func state(t *testing.T, f *Fixture) State {
	t.Helper()
	ctx := context.Background()
	var s State
	var reason *string
	q := f.H.Admin
	if err := q.QueryRow(ctx, `SELECT status, control_epoch, available_minor, reserved_minor, committed_minor, unresolved_minor
		FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`, f.Tenant, f.Mandate).
		Scan(&s.MandateStatus, &s.Epoch, &s.Available, &s.Reserved, &s.Committed, &s.Unresolved); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(ctx, `SELECT status, release_reason FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`,
		f.Tenant, f.Permit).Scan(&s.IssuanceStatus, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		s.ReleaseReason = *reason
	}
	for _, c := range []struct {
		dst *int
		sql string
	}{
		{&s.Orders, `SELECT count(*) FROM target.purchase_orders`},
		{&s.Obligations, `SELECT count(*) FROM steelmatter.obligations`},
		{&s.Operations, `SELECT count(*) FROM steelmatter.operations`},
		{&s.OutboxEvents, `SELECT count(*) FROM steelmatter.outbox`},
	} {
		if err := q.QueryRow(ctx, c.sql).Scan(c.dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`,
		f.Tenant, f.Instruction).Scan(&s.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if s.Available+s.Reserved+s.Committed+s.Unresolved != limit || s.Available < 0 || s.Reserved < 0 || s.Committed < 0 {
		t.Fatalf("I3 violated: %+v", s)
	}
	return s
}

// Op runs inside a transaction and returns an outcome; success outcomes commit,
// rejections roll back.
type Op func(ctx context.Context, tx pgx.Tx, hook Hook) (string, error)

var commits = map[string]bool{Committed: true, Reserved: true, Revoked: true, Released: true, Amended: true, SignaturePersisted: true}

func runTx(ctx context.Context, conn *pgxpool.Conn, op Op, hook Hook) (string, error) {
	tx, err := conn.Begin(ctx) // READ COMMITTED (PostgreSQL default)
	if err != nil {
		return "", err
	}
	out, err := op(ctx, tx, hook)
	if err != nil || !commits[out] {
		_ = tx.Rollback(ctx)
		return out, err
	}
	return out, tx.Commit(ctx)
}

// Race runs first until it holds its locks, starts second, waits until the
// database reports second blocked on a lock, then lets first finish. The
// ordering is established by the barrier and pg_stat_activity, not by sleeps.
func Race(t *testing.T, f *Fixture, first, second Op) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c1, err := f.H.Executor.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Release()
	c2, err := f.H.Executor.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Release()
	var pid2 int32
	if err := c2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid2); err != nil {
		t.Fatal(err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	type result struct {
		out string
		err error
	}
	r1, r2 := make(chan result, 1), make(chan result, 1)
	go func() {
		out, err := runTx(ctx, c1, first, func() { close(held); <-release })
		r1 <- result{out, err}
	}()
	select {
	case <-held:
	case r := <-r1:
		t.Fatalf("first finished before taking its locks: %v %v", r.out, r.err)
	}
	go func() {
		out, err := runTx(ctx, c2, second, noHook)
		r2 <- result{out, err}
	}()
	waitBlocked(t, ctx, f.H.Admin, pid2)
	close(release)
	a, b := <-r1, <-r2
	if a.err != nil || b.err != nil {
		t.Fatalf("race errors: first=%v second=%v", a.err, b.err)
	}
	return a.out, b.out
}

func waitBlocked(t *testing.T, ctx context.Context, admin *pgxpool.Pool, pid int32) {
	t.Helper()
	for {
		var waiting bool
		err := admin.QueryRow(ctx, `SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("second transaction never blocked on a lock")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func admit(f *Fixture, times *Times) Op {
	return func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		return Admit(ctx, tx, f, f.Permit, h, times)
	}
}
func revoke(f *Fixture) Op {
	return func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) { return Revoke(ctx, tx, f, h) }
}
func cleanup(f *Fixture) Op {
	return func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) { return Cleanup(ctx, tx, f, f.Permit, h) }
}
func amend(f *Fixture, id string, expected int64) Op {
	return func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		return Amend(ctx, tx, f, id, expected, h)
	}
}

func expect(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %s, want %s", what, got, want)
	}
}

func expectState(t *testing.T, got State, want State) {
	t.Helper()
	if got != want {
		t.Fatalf("state\n got %+v\nwant %+v", got, want)
	}
}

// --- Revocation vs admission (T07 ordering) ---------------------------------

func TestPrototypeRevocationBeforeAdmission(t *testing.T) {
	f := seed(t, seedOpts{})
	r, a := Race(t, f, revoke(f), admit(f, nil))
	expect(t, "revocation", r, Revoked)
	expect(t, "admission", a, AuthorityRevoked)
	expectState(t, state(t, f), State{MandateStatus: "REVOKED", Epoch: 2, Available: limit, IssuanceStatus: "RELEASED", ReleaseReason: "REVOKED", CurrentRevision: 1})
}

func TestPrototypeAdmissionBeforeRevocation(t *testing.T) {
	f := seed(t, seedOpts{})
	a, r := Race(t, f, admit(f, nil), revoke(f))
	expect(t, "admission", a, Committed)
	expect(t, "revocation", r, Revoked)
	// Revocation after commit cannot undo the order (I6).
	expectState(t, state(t, f), State{MandateStatus: "REVOKED", Epoch: 2, Available: limit - amount, Committed: amount,
		IssuanceStatus: "CONSUMED", Orders: 1, Obligations: 1, Operations: 1, OutboxEvents: 1, CurrentRevision: 1})
}

// --- Cleanup vs admission (T28 ordering) ------------------------------------

func TestPrototypeCleanupBeforeAdmission(t *testing.T) {
	f := seed(t, seedOpts{permitExpiresIn: -time.Second})
	c, a := Race(t, f, cleanup(f), admit(f, nil))
	expect(t, "cleanup", c, Released)
	expect(t, "admission", a, PermitNotActive)
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit, IssuanceStatus: "RELEASED", ReleaseReason: "EXPIRED", CurrentRevision: 1})
}

func TestPrototypeAdmissionBeforeCleanup(t *testing.T) {
	f := seed(t, seedOpts{})
	a, c := Race(t, f, admit(f, nil), cleanup(f))
	expect(t, "admission", a, Committed)
	expect(t, "cleanup", c, SkippedNotUnused)
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit - amount, Committed: amount,
		IssuanceStatus: "CONSUMED", Orders: 1, Obligations: 1, Operations: 1, OutboxEvents: 1, CurrentRevision: 1})
}

// --- Expiry after lock wait (T08) -------------------------------------------

// A lock holder keeps the mandate locked until the permit has expired. The
// admission was started (BEGIN) before expiry, so now() is still before
// expires_at; only the clock_timestamp() recheck after the wait rejects it.
func TestPrototypeExpiryAfterLockWait(t *testing.T) {
	f := seed(t, seedOpts{permitExpiresIn: 1500 * time.Millisecond})
	var times Times
	holder := func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, f.Tenant, f.Mandate); err != nil {
			return "", err
		}
		h()
		return "HELD", nil
	}
	// The holder's hook returns only after the second transaction is blocked;
	// extend it until the database clock passes the permit expiry.
	waitingHolder := func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		return holder(ctx, tx, func() {
			h()
			for {
				var expired bool
				if err := f.H.Admin.QueryRow(ctx, `SELECT clock_timestamp() >= expires_at FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`,
					f.Tenant, f.Permit).Scan(&expired); err != nil || expired {
					return
				}
				time.Sleep(20 * time.Millisecond) // waiting for wall-clock expiry, not for ordering
			}
		})
	}
	h, a := Race(t, f, waitingHolder, admit(f, &times))
	expect(t, "holder", h, "HELD")
	expect(t, "admission", a, PermitExpired)
	if times.ExpiredByNow || !times.Expired {
		t.Fatalf("expected now() before expiry and clock_timestamp() after it; got %+v", times)
	}
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit - amount, Reserved: amount, IssuanceStatus: "ACTIVE", CurrentRevision: 1})
}

// --- Amendment vs admission and late signature (T10 orderings) ---------------

func TestPrototypeAmendmentBeforeAdmission(t *testing.T) {
	f := seed(t, seedOpts{})
	m, a := Race(t, f, amend(f, "amend-1", 1), admit(f, nil))
	expect(t, "amendment", m, Amended)
	expect(t, "admission", a, RevisionStale)
	// Unused reservation released exactly once; new revision has no reservation.
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit, IssuanceStatus: "RELEASED", ReleaseReason: "AMENDED", CurrentRevision: 2})
}

func TestPrototypeAdmissionBeforeAmendment(t *testing.T) {
	f := seed(t, seedOpts{})
	a, m := Race(t, f, admit(f, nil), amend(f, "amend-1", 1))
	expect(t, "admission", a, Committed)
	expect(t, "amendment", m, AlreadyCommitted)
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit - amount, Committed: amount,
		IssuanceStatus: "CONSUMED", Orders: 1, Obligations: 1, Operations: 1, OutboxEvents: 1, CurrentRevision: 1})
}

func TestPrototypeLateSignatureAfterAmendment(t *testing.T) {
	f := seed(t, seedOpts{issuanceStatus: "UNSIGNED"})
	persist := func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		return PersistSignature(ctx, tx, f, f.Permit, "late.signature.value", h)
	}
	m, p := Race(t, f, amend(f, "amend-1", 1), persist)
	expect(t, "amendment", m, Amended)
	expect(t, "late signature", p, SignatureDiscarded)
	var jws *string
	if err := f.H.Admin.QueryRow(context.Background(), `SELECT permit_jws FROM steelmatter.issuances WHERE permit_id = $1`, f.Permit).Scan(&jws); err != nil {
		t.Fatal(err)
	}
	if jws != nil {
		t.Fatal("late signature was published for an invalidated issuance")
	}
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit, IssuanceStatus: "RELEASED", ReleaseReason: "AMENDED", CurrentRevision: 2})
}

func TestPrototypeSignatureBeforeAmendment(t *testing.T) {
	f := seed(t, seedOpts{issuanceStatus: "UNSIGNED"})
	persist := func(ctx context.Context, tx pgx.Tx, h Hook) (string, error) {
		return PersistSignature(ctx, tx, f, f.Permit, "timely.signature.value", h)
	}
	p, m := Race(t, f, persist, amend(f, "amend-1", 1))
	expect(t, "signature", p, SignaturePersisted)
	expect(t, "amendment", m, Amended)
	// The amendment still invalidates the now-active permit and releases once.
	expectState(t, state(t, f), State{MandateStatus: "ACTIVE", Epoch: 1, Available: limit, IssuanceStatus: "RELEASED", ReleaseReason: "AMENDED", CurrentRevision: 2})
}

// --- Shared allowance (T01 shape) -------------------------------------------

func TestPrototypeConcurrentReservations(t *testing.T) {
	h := dbtest.New(t)
	ctx := context.Background()
	f := &Fixture{H: h, Tenant: "synthetic-org", Mandate: "mandate-001"}
	for _, s := range []string{
		`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind) VALUES ('owner-a','synthetic-org','OWNER'),('issuer-a','synthetic-org','ISSUER'),('agent-a','synthetic-org','ACTOR'),('agent-b','synthetic-org','ACTOR')`,
		fmt.Sprintf(`INSERT INTO steelmatter.mandates (tenant_id, mandate_id, revision, control_epoch, status, owner_principal, audience, actor_ids, supplier_ids, delivery_ids,
			currency, limit_minor, available_minor, reserved_minor, committed_minor, payload_canonical, mandate_digest, issued_at, expires_at)
			VALUES ('synthetic-org','mandate-001',1,1,'ACTIVE','owner-a','synthetic-procurement','{agent-a,agent-b}','{supplier-a}','{warehouse-a}','INR',%d,%d,0,0,'\x7b7d'::bytea,repeat('1',64),now()-interval '1 hour',now()+interval '1 hour')`, limit, limit),
	} {
		if _, err := h.Admin.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []struct{ id, actor string }{{"inst-a", "agent-a"}, {"inst-b", "agent-b"}} {
		if _, err := h.Admin.Exec(ctx, `INSERT INTO steelmatter.instructions (tenant_id, instruction_id, source_system, source_record_id, mandate_id, actor_id, issuer_principal, creation_request_digest, current_revision, created_at)
			VALUES ('synthetic-org', $1, 'synthetic-erp', $1, 'mandate-001', $2, 'issuer-a', repeat('e',64), 1, now())`, i.id, i.actor); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Admin.Exec(ctx, `INSERT INTO steelmatter.action_revisions (tenant_id, instruction_id, action_revision, status, action_canonical, action_digest, supplier_id, supplier_revision, delivery_id, audience, amount_minor, created_at)
			VALUES ('synthetic-org', $1, 1, 'CURRENT', '\x7b7d'::bytea, repeat('0',64), 'supplier-a', 1, 'warehouse-a', 'synthetic-procurement', $2, now())`, i.id, amount); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(instr, permit string) Op {
		return func(ctx context.Context, tx pgx.Tx, hk Hook) (string, error) {
			return Reserve(ctx, tx, f, instr, permit, amount, hk)
		}
	}
	a, b := Race(t, f, reserve("inst-a", "permit-a"), reserve("inst-b", "permit-b"))
	expect(t, "first reservation", a, Reserved)
	expect(t, "second reservation", b, CapacityExhausted)
	var avail, reserved, n int64
	if err := h.Admin.QueryRow(ctx, `SELECT available_minor, reserved_minor FROM steelmatter.mandates`).Scan(&avail, &reserved); err != nil {
		t.Fatal(err)
	}
	if err := h.Admin.QueryRow(ctx, `SELECT count(*) FROM steelmatter.issuances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if avail != 40_000_000 || reserved != amount || n != 1 {
		t.Fatalf("available %d reserved %d issuances %d; want 40,000,000 / 60,000,000 / 1", avail, reserved, n)
	}
}

// --- Constraints as a backstop ----------------------------------------------

func TestPrototypeConstraintsRejectInvariantBreaks(t *testing.T) {
	f := seed(t, seedOpts{})
	ctx := context.Background()
	cases := []struct {
		name, sql, code string
	}{
		{"bucket sum", `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor + 1`, "23514"},
		{"negative bucket", `UPDATE steelmatter.mandates SET available_minor = -1, reserved_minor = reserved_minor + available_minor + 1`, "23514"},
		{"second live issuance for a revision", `INSERT INTO steelmatter.issuances (tenant_id, permit_id, reservation_id, operation_id, instruction_id, action_revision, mandate_id, control_epoch, amount_minor, status, permit_canonical, permit_digest, issued_at, expires_at)
			VALUES ('synthetic-org','permit-dup','res-dup','op-dup','inst-001',1,'mandate-001',1,1,'UNSIGNED','\x7b7d'::bytea,repeat('9',64),now(),now()+interval '1 minute')`, "23505"},
		// Since 0005 the executor cannot update the permit window at all, and a
		// persisted signature is write-once (stronger than the CHECK constraints).
		{"permit longer than 5 minutes", `UPDATE steelmatter.issuances SET expires_at = issued_at + interval '6 minutes'`, "42501"},
		{"active without signature", `UPDATE steelmatter.issuances SET permit_jws = NULL`, "SM001"},
		{"second order for an instruction", `INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id, supplier_revision, delivery_id, currency, amount_minor, created_at)
			VALUES ('synthetic-org','po-x','inst-001','op-x','supplier-a',1,'warehouse-a','INR',1,now()), ('synthetic-org','po-y','inst-001','op-y','supplier-a',1,'warehouse-a','INR',1,now())`, "23505"},
		{"duplicate source record", `INSERT INTO steelmatter.instructions (tenant_id, instruction_id, source_system, source_record_id, mandate_id, actor_id, issuer_principal, creation_request_digest, current_revision, created_at)
			VALUES ('synthetic-org','inst-002','synthetic-erp','po-request-001','mandate-001','agent-a','issuer-a',repeat('e',64),1,now())`, "23505"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.H.Executor.Exec(ctx, c.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != c.code {
				t.Fatalf("got %v; want SQLSTATE %s", err, c.code)
			}
		})
	}
	state(t, f) // unchanged and conserved
}

// Executor cannot delete history or change protected supplier data; observer
// cannot write anything (T22/T25 database-layer portion).
func TestPrototypeRolePrivileges(t *testing.T) {
	f := seed(t, seedOpts{})
	ctx := context.Background()
	denied := []struct {
		name string
		pool *pgxpool.Pool
		sql  string
	}{
		{"executor DELETE issuance", f.H.Executor, `DELETE FROM steelmatter.issuances`},
		{"executor DELETE order", f.H.Executor, `DELETE FROM target.purchase_orders`},
		{"executor change supplier revision", f.H.Executor, `UPDATE target.suppliers SET revision = revision + 1`},
		{"executor change workflow assignment", f.H.Executor, `UPDATE steelmatter.workflow_assignments SET enabled = false`},
		{"observer insert order", f.H.Observer, `INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id, supplier_revision, delivery_id, currency, amount_minor, created_at) VALUES ('synthetic-org','po-o','inst-o','op-o','supplier-a',1,'warehouse-a','INR',1,now())`},
		{"observer update budget", f.H.Observer, `UPDATE steelmatter.mandates SET available_minor = available_minor`},
		{"observer read mandates", f.H.Observer, `SELECT * FROM steelmatter.mandates`},
	}
	for _, c := range denied {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.pool.Exec(ctx, c.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("got %v; want permission denied (42501)", err)
			}
		})
	}
	// The executor can take the supplier FOR SHARE lock admission needs.
	tx, err := f.H.Executor.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM target.suppliers FOR SHARE`); err != nil {
		t.Fatalf("executor FOR SHARE on supplier: %v", err)
	}
}
