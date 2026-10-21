package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey serialises concurrent migrators on one database.
const migrationLockKey int64 = 0x53544d4d // "STMM"

// Migration is one embedded SQL file named NNNN_description.sql.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration %q must be named NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("migration %q has invalid version", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, name, v)
		}
		seen[v] = name
		body, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{Version: v, Name: name, SQL: string(body), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies pending migrations in one transaction. It must run as the
// database administrator (object owner), never as the executor or observer.
// An applied migration whose file content changed is an error: history is
// append-only.
func Migrate(ctx context.Context, conn *pgx.Conn) (applied []string, err error) {
	return MigrateUpTo(ctx, conn, 0)
}

// MigrateUpTo applies pending migrations with version <= upTo (0 = all). It
// exists so tests can reproduce a database migrated before a later fix.
func MigrateUpTo(ctx context.Context, conn *pgx.Conn, upTo int) (applied []string, err error) {
	migs, err := Migrations()
	if err != nil {
		return nil, err
	}
	if err := EnsureGroupRoles(ctx, conn); err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS steelmatter_meta;
		REVOKE ALL ON SCHEMA steelmatter_meta FROM PUBLIC;
		CREATE TABLE IF NOT EXISTS steelmatter_meta.migrations (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create migration table: %w", err)
	}
	done := map[int]string{}
	rows, err := tx.Query(ctx, `SELECT version, checksum FROM steelmatter_meta.migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		var sum string
		if err = rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return nil, err
		}
		done[v] = sum
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range migs {
		if upTo > 0 && m.Version > upTo {
			break
		}
		if sum, ok := done[m.Version]; ok {
			if sum != m.Checksum {
				return nil, fmt.Errorf("migration %s was modified after it was applied", m.Name)
			}
			continue
		}
		if _, err = tx.Exec(ctx, m.SQL); err != nil {
			return nil, fmt.Errorf("apply %s: %w", m.Name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO steelmatter_meta.migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Checksum); err != nil {
			return nil, err
		}
		applied = append(applied, m.Name)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return applied, nil
}
