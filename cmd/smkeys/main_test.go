package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
)

// SEC-DEF-004: retiring a key again with a later time must not reactivate it.
// The refused attempt leaves the trust store file unchanged.
func TestRetireRefusesReactivation(t *testing.T) {
	dir := t.TempDir()
	created := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	if _, err := keys.Provision(dir, "steelmatter-service", []string{"SM-PERMIT+JWS"}, created); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trust-store.json")
	retired := created.Add(time.Hour)
	now := retired.Add(30 * time.Minute) // retired 30 minutes ago

	var out strings.Builder
	ts, _ := keys.LoadTrustStore(path)
	if err := retire(ts, path, "steelmatter-service", retired, retired, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not_after none -> 2026-10-12T10:00:00Z") {
		t.Fatalf("output %q", out.String())
	}
	before, _ := os.ReadFile(path)

	out.Reset()
	ts, _ = keys.LoadTrustStore(path)
	err := retire(ts, path, "steelmatter-service", now.Add(time.Hour), now, &out)
	if !errors.Is(err, keys.ErrRetirementEffective) || !strings.Contains(err.Error(), "2026-10-12T10:00:00Z") {
		t.Fatalf("reactivation not refused: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || out.Len() != 0 {
		t.Fatal("a refused retirement changed the trust store or reported success")
	}
	ts, _ = keys.LoadTrustStore(path)
	if !ts.Keys[0].RetiredAt(now) {
		t.Fatal("key is no longer retired")
	}

	// Moving it earlier is allowed and shows both values.
	if err := retire(ts, path, "steelmatter-service", retired.Add(-30*time.Minute), now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not_after 2026-10-12T10:00:00Z -> 2026-10-12T09:30:00Z") {
		t.Fatalf("output %q", out.String())
	}
}
