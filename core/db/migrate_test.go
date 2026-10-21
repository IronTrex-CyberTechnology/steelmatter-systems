package db

import (
	"context"
	"testing"
)

func TestEmbeddedMigrationsAreContiguousAndChecksummed(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i, m := range migs {
		if m.Version != i+1 {
			t.Fatalf("migration %s has version %d; want %d (no gaps)", m.Name, m.Version, i+1)
		}
		if len(m.Checksum) != 64 {
			t.Fatalf("migration %s checksum %q", m.Name, m.Checksum)
		}
	}
}

func TestCreateLoginRoleRejectsNonGeneratedPassword(t *testing.T) {
	for _, pw := range []string{"", "x' SUPERUSER --", "ABCDEF"} {
		if err := CreateLoginRole(context.Background(), nil, "r", pw, GroupExecutor); err == nil {
			t.Errorf("password %q accepted", pw)
		}
	}
}

func TestRandomPasswordIsHex(t *testing.T) {
	pw, err := RandomPassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 64 || !isHex(pw) {
		t.Fatalf("password shape %d chars", len(pw))
	}
}
