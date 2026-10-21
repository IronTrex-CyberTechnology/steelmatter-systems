//go:build integration

package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/db"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/dbtest"
)

const insufficientPrivilege = "42501"

func requireSQLState(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected SQLSTATE %s, got %v", what, code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %s (%s)", what, code, pgErr.Code, pgErr.Message)
	}
}

func TestHarnessMigrationsApplyOnceAndAreIdempotent(t *testing.T) {
	h := dbtest.New(t)
	ctx := context.Background()

	migs, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.Admin.QueryRow(ctx, `SELECT count(*) FROM steelmatter_meta.migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migs) {
		t.Fatalf("applied %d migrations, embedded %d", n, len(migs))
	}

	conn, err := pgx.Connect(ctx, h.DB.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	applied, err := db.Migrate(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v; want none", applied)
	}

	var baseline string
	if err := h.Executor.QueryRow(ctx, `SELECT value FROM steelmatter.schema_info WHERE key = 'baseline'`).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != "0.6" {
		t.Fatalf("baseline = %q", baseline)
	}
}

func TestHarnessPoolsUseDistinctRoles(t *testing.T) {
	h := dbtest.New(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		pool interface {
			QueryRow(context.Context, string, ...any) pgx.Row
		}
		want string
	}{{"executor", h.Executor, h.DB.ExecutorRole}, {"observer", h.Observer, h.DB.ObserverRole}} {
		var user string
		var super bool
		if err := c.pool.QueryRow(ctx, `SELECT current_user, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super); err != nil {
			t.Fatal(err)
		}
		if user != c.want || super {
			t.Fatalf("%s connected as %q (superuser=%v); want non-superuser %q", c.name, user, super, c.want)
		}
	}
}

func TestHarnessExecutorCannotRunDDLOrReadMigrationHistory(t *testing.T) {
	h := dbtest.New(t)
	ctx := context.Background()

	_, err := h.Executor.Exec(ctx, `CREATE TABLE steelmatter.executor_probe (id int)`)
	requireSQLState(t, err, insufficientPrivilege, "executor CREATE TABLE in steelmatter")

	_, err = h.Executor.Exec(ctx, `CREATE TABLE public.executor_probe (id int)`)
	requireSQLState(t, err, insufficientPrivilege, "executor CREATE TABLE in public")

	_, err = h.Executor.Exec(ctx, `UPDATE steelmatter.schema_info SET value = 'x' WHERE key = 'baseline'`)
	requireSQLState(t, err, insufficientPrivilege, "executor UPDATE schema_info")

	_, err = h.Executor.Exec(ctx, `SELECT * FROM steelmatter_meta.migrations`)
	requireSQLState(t, err, insufficientPrivilege, "executor read migration history")
}

func TestHarnessObserverIsReadOnly(t *testing.T) {
	h := dbtest.New(t)
	ctx := context.Background()

	var n int
	if err := h.Observer.QueryRow(ctx, `SELECT count(*) FROM target.suppliers`).Scan(&n); err != nil {
		t.Fatalf("observer read: %v", err)
	}

	for _, c := range []struct{ sql, what string }{
		{`INSERT INTO steelmatter.schema_info (key, value) VALUES ('observer', 'write')`, "observer INSERT"},
		{`UPDATE steelmatter.schema_info SET value = 'x'`, "observer UPDATE"},
		{`DELETE FROM steelmatter.schema_info`, "observer DELETE"},
		{`CREATE TABLE steelmatter.observer_probe (id int)`, "observer CREATE TABLE"},
	} {
		_, err := h.Observer.Exec(ctx, c.sql)
		requireSQLState(t, err, insufficientPrivilege, c.what)
	}

	var after int
	if err := h.Admin.QueryRow(ctx, `SELECT count(*) FROM steelmatter.schema_info`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 2 {
		t.Fatalf("schema_info rows changed to %d", after)
	}
}

func TestHarnessLoginRolesCannotConnectToOtherTestDatabases(t *testing.T) {
	a := dbtest.New(t)
	b := dbtest.New(t)
	ctx := context.Background()

	cfg, err := pgx.ParseConfig(a.DB.ExecutorURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = b.DB.Name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err == nil {
		conn.Close(ctx)
		t.Fatalf("executor of %s connected to %s", a.DB.Name, b.DB.Name)
	}
	requireSQLState(t, err, insufficientPrivilege, "cross-database CONNECT")
}

// approvedObserverReads is the observer's complete read set (I02 §8,
// docs/evidence.md; QA-D1-001).
var approvedObserverReads = []string{
	"steelmatter.action_revisions", "steelmatter.obligations", "steelmatter.operations",
	"target.purchase_order_lines", "target.purchase_orders", "target.suppliers",
}

// observerPrivileges lists every Steelmatter table the role can read (any
// column) and every table it can write in any way.
func observerPrivileges(t *testing.T, ctx context.Context, admin *pgx.Conn, role string) (reads, writes []string) {
	t.Helper()
	rows, err := admin.Query(ctx, `SELECT n.nspname || '.' || c.relname,
		has_any_column_privilege($1, c.oid, 'SELECT'),
		has_any_column_privilege($1, c.oid, 'INSERT') OR has_any_column_privilege($1, c.oid, 'UPDATE')
		  OR has_table_privilege($1, c.oid, 'DELETE') OR has_table_privilege($1, c.oid, 'TRUNCATE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname IN ('steelmatter', 'steelmatter_meta', 'target') ORDER BY 1`, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var read, write bool
		if err := rows.Scan(&name, &read, &write); err != nil {
			t.Fatal(err)
		}
		if read {
			reads = append(reads, name)
		}
		if write {
			writes = append(writes, name)
		}
	}
	return reads, writes
}

func requireExactObserverReadSet(t *testing.T, ctx context.Context, d *db.Database, observerURL string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, d.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	reads, writes := observerPrivileges(t, ctx, admin, d.ObserverRole)
	if strings.Join(reads, ",") != strings.Join(approvedObserverReads, ",") {
		t.Fatalf("observer read set\n got %v\nwant %v", reads, approvedObserverReads)
	}
	if len(writes) != 0 {
		t.Fatalf("observer can write %v", writes)
	}
	// The approved reads still work for the observer login itself.
	obs, err := pgx.Connect(ctx, observerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close(ctx)
	for _, tb := range approvedObserverReads {
		if _, err := obs.Exec(ctx, "SELECT 1 FROM "+tb+" LIMIT 1"); err != nil {
			t.Fatalf("observer cannot read approved table %s: %v", tb, err)
		}
	}
	for _, tb := range []string{"steelmatter.schema_info", "steelmatter.event_receipts", "steelmatter.observations", "steelmatter.mandates", "steelmatter.principals"} {
		_, err := obs.Exec(ctx, "SELECT 1 FROM "+tb+" LIMIT 1")
		requireSQLState(t, err, insufficientPrivilege, "observer reads "+tb)
	}
}

// QA-D1-001 regression on a fresh database: the observer reads exactly the
// approved tables and writes none.
func TestObserverExactReadSetFreshDatabase(t *testing.T) {
	h := dbtest.New(t)
	requireExactObserverReadSet(t, context.Background(), h.DB, h.DB.ObserverURL)
}

// QA-D1-001 regression on an existing database migrated before the fix
// (through 0005): it has the extra grants, and migrating it applies 0006 and
// leaves exactly the approved read set.
func TestObserverExactReadSetAfterUpgrade(t *testing.T) {
	adminURL := os.Getenv(dbtest.EnvAdminURL)
	if adminURL == "" {
		t.Skipf("%s is not set", dbtest.EnvAdminURL)
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	d, err := db.ProvisionUpTo(ctx, adminURL, "sm_test_up_"+hex.EncodeToString(suffix), 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Drop(context.Background(), adminURL, d) })

	admin, err := pgx.Connect(ctx, d.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	before, _ := observerPrivileges(t, ctx, admin, d.ObserverRole)
	if len(before) != len(approvedObserverReads)+3 {
		t.Fatalf("pre-fix database should show the three extra grants; got %v", before)
	}
	// Upgrade through 0006 only; later migrations are unrelated to this fix.
	applied, err := db.MigrateUpTo(ctx, admin, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "0006_observer_read_set.sql" {
		t.Fatalf("upgrade applied %v", applied)
	}
	requireExactObserverReadSet(t, ctx, d, d.ObserverURL)
}
