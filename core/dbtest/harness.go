// Package dbtest provides a disposable real-PostgreSQL database per test run.
//
// Set STEELMATTER_TEST_ADMIN_URL to a postgres:// URL for a role with CREATEDB
// and CREATEROLE (local server, scripts/dev-postgres container or the CI
// service container). Each harness creates its own database and login roles
// and drops them afterwards. Set STEELMATTER_TEST_KEEP_DB=1 to keep the
// database for inspection; its name is logged.
//
// When the admin URL is absent the test is skipped. The CI aggregate treats a
// skipped required test as a failure, so a missing database cannot pass CI.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/db"
)

const (
	EnvAdminURL = "STEELMATTER_TEST_ADMIN_URL"
	EnvKeepDB   = "STEELMATTER_TEST_KEEP_DB"
)

// Harness holds connection pools for each database role.
type Harness struct {
	DB       *db.Database
	Admin    *pgxpool.Pool // object owner; use only for setup and state inspection
	Executor *pgxpool.Pool
	Observer *pgxpool.Pool
}

// New provisions a fresh migrated database and registers cleanup on t.
func New(t testing.TB) *Harness {
	t.Helper()
	adminURL := os.Getenv(EnvAdminURL)
	if adminURL == "" {
		t.Skipf("%s is not set; real PostgreSQL is required", EnvAdminURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "sm_test_" + hex.EncodeToString(suffix)
	d, err := db.Provision(ctx, adminURL, name)
	if err != nil {
		t.Fatalf("provision test database: %v", err)
	}
	h := &Harness{DB: d}
	t.Cleanup(func() {
		for _, p := range []*pgxpool.Pool{h.Admin, h.Executor, h.Observer} {
			if p != nil {
				p.Close()
			}
		}
		if os.Getenv(EnvKeepDB) == "1" {
			t.Logf("kept test database %s (roles %s, %s)", d.Name, d.ExecutorRole, d.ObserverRole)
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := db.Drop(cctx, adminURL, d); err != nil {
			t.Errorf("drop test database %s: %v", d.Name, err)
		}
	})

	for _, c := range []struct {
		dst **pgxpool.Pool
		url string
	}{{&h.Admin, d.AdminURL}, {&h.Executor, d.ExecutorURL}, {&h.Observer, d.ObserverURL}} {
		p, err := pgxpool.New(ctx, c.url)
		if err != nil {
			t.Fatalf("open pool: %v", err)
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			t.Fatalf("ping pool: %v", err)
		}
		*c.dst = p
	}
	t.Logf("test database %s", d.Name)
	return h
}
