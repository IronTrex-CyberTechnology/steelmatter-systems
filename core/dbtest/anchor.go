package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
)

// AnchorHarness holds pools for a disposable recovery anchor: a separate
// database with its own roles, never restored with the engine database.
type AnchorHarness struct {
	DB       *anchor.Database
	Admin    *pgxpool.Pool // owner; setup, inspection and tamper tests only
	Engine   *pgxpool.Pool // anchor_engine_writer
	Dest     *pgxpool.Pool // anchor_dest_writer (the Odoo module's identity)
	Observer *pgxpool.Pool // anchor_observer
}

// NewAnchor provisions a fresh anchor database and registers cleanup on t.
func NewAnchor(t testing.TB) *AnchorHarness {
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
	d, err := anchor.Provision(ctx, adminURL, "sm_anchor_"+hex.EncodeToString(suffix))
	if err != nil {
		t.Fatalf("provision anchor: %v", err)
	}
	h := &AnchorHarness{DB: d}
	t.Cleanup(func() {
		for _, p := range []*pgxpool.Pool{h.Admin, h.Engine, h.Dest, h.Observer} {
			if p != nil {
				p.Close()
			}
		}
		if os.Getenv(EnvKeepDB) == "1" {
			t.Logf("kept anchor database %s", d.Name)
			return
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := anchor.Drop(cctx, adminURL, d); err != nil {
			t.Errorf("drop anchor %s: %v", d.Name, err)
		}
	})
	for _, c := range []struct {
		dst **pgxpool.Pool
		url string
	}{{&h.Admin, d.AdminURL}, {&h.Engine, d.EngineURL}, {&h.Dest, d.DestURL}, {&h.Observer, d.ObserverURL}} {
		p, err := pgxpool.New(ctx, c.url)
		if err != nil {
			t.Fatalf("open anchor pool: %v", err)
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			t.Fatalf("ping anchor pool: %v", err)
		}
		*c.dst = p
	}
	t.Logf("anchor database %s", d.Name)
	return h
}
