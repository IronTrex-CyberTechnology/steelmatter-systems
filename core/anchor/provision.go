// Package anchor is the Steelmatter recovery anchor (D5 v3, decided 28 Oct
// 2026, option 1): a separate PostgreSQL database, outside the rollback
// domain of both the engine database and Odoo, holding append-only,
// hash-chained records from which the authorised request, the budget move and
// the outcome of every R1 operation can be recovered.
//
// It runs locally for now (no paid hosting). Its backups must never be
// restored together with the engine's or Odoo's: restoring Steelmatter or
// Odoo leaves the anchor's purchase history untouched.
package anchor

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/db"
)

//go:embed schema.sql
var schemaSQL string

//go:embed append_destination.sql
var appendDestinationSQL string

// Group roles (NOLOGIN, cluster-wide). Login roles per anchor database
// inherit from one of them.
const (
	GroupEngineWriter = "anchor_engine_writer"
	GroupDestWriter   = "anchor_dest_writer"
	GroupObserver     = "anchor_observer"
)

// Database describes a provisioned anchor and its three login roles. The
// URLs embed generated passwords; never log them.
type Database struct {
	Name         string
	AdminURL     string
	EngineRole   string
	EngineURL    string
	DestRole     string
	DestURL      string
	ObserverRole string
	ObserverURL  string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Provision creates anchor database name with its schema and three login
// roles. adminURL must be a postgres:// URL for a role with CREATEDB and
// CREATEROLE. CONNECT is revoked from PUBLIC.
func Provision(ctx context.Context, adminURL, name string) (*Database, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("anchor database name %q must match %s", name, namePattern)
	}
	base, err := url.Parse(adminURL)
	if err != nil || (base.Scheme != "postgres" && base.Scheme != "postgresql") {
		return nil, errors.New("admin URL must be a postgres:// URL")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return nil, fmt.Errorf("connect admin: %w", err)
	}
	defer admin.Close(ctx)
	for _, g := range []string{GroupEngineWriter, GroupDestWriter, GroupObserver} {
		if err := ensureGroup(ctx, admin, g); err != nil {
			return nil, err
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident(name)); err != nil {
		return nil, fmt.Errorf("create anchor database: %w", err)
	}
	d := &Database{Name: name, EngineRole: name + "_engine", DestRole: name + "_dest", ObserverRole: name + "_observer"}
	cleanup := func() { _ = Drop(context.WithoutCancel(ctx), adminURL, d) }
	pw := map[string]string{}
	for _, r := range []struct{ role, group string }{{d.EngineRole, GroupEngineWriter}, {d.DestRole, GroupDestWriter}, {d.ObserverRole, GroupObserver}} {
		p, err := db.RandomPassword()
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := db.CreateLoginRole(ctx, admin, r.role, p, r.group); err != nil {
			cleanup()
			return nil, err
		}
		pw[r.role] = p
	}
	for _, s := range []string{
		"REVOKE CONNECT, TEMPORARY ON DATABASE " + ident(name) + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + ident(name) + " TO " + ident(d.EngineRole) + ", " + ident(d.DestRole) + ", " + ident(d.ObserverRole),
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			cleanup()
			return nil, fmt.Errorf("anchor database privileges: %w", err)
		}
	}
	d.AdminURL = withDatabase(base, name, nil)
	d.EngineURL = withDatabase(base, name, url.UserPassword(d.EngineRole, pw[d.EngineRole]))
	d.DestURL = withDatabase(base, name, url.UserPassword(d.DestRole, pw[d.DestRole]))
	d.ObserverURL = withDatabase(base, name, url.UserPassword(d.ObserverRole, pw[d.ObserverRole]))

	target, err := pgx.Connect(ctx, d.AdminURL)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("connect anchor database: %w", err)
	}
	defer target.Close(ctx)
	if _, err := target.Exec(ctx, schemaSQL); err != nil {
		cleanup()
		return nil, fmt.Errorf("anchor schema: %w", err)
	}
	if _, err := target.Exec(ctx, appendDestinationSQL); err != nil {
		cleanup()
		return nil, fmt.Errorf("anchor schema: %w", err)
	}
	return d, nil
}

// Upgrade brings an existing anchor's functions up to date without touching
// any record: it replaces anchor.append_destination (R1-DEF-14: no new
// CREATE_INTENT while engine writes are off). anchorAdminURL is a postgres://
// URL for the anchor database's owner. Safe to run more than once.
func Upgrade(ctx context.Context, anchorAdminURL string) error {
	conn, err := pgx.Connect(ctx, anchorAdminURL)
	if err != nil {
		return fmt.Errorf("connect anchor database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, appendDestinationSQL); err != nil {
		return fmt.Errorf("anchor upgrade: %w", err)
	}
	return nil
}

// Drop removes the anchor database and its login roles. Missing objects are
// ignored. Only for disposable local and test anchors.
func Drop(ctx context.Context, adminURL string, d *Database) error {
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	var errs []error
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(d.Name)+" WITH (FORCE)"); err != nil {
		errs = append(errs, err)
	}
	for _, r := range []string{d.EngineRole, d.DestRole, d.ObserverRole} {
		if r == "" {
			continue
		}
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+ident(r)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ensureGroup(ctx context.Context, q db.Querier, role string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return fmt.Errorf("check role %s: %w", role, err)
	}
	if exists {
		return nil
	}
	if _, err := q.Exec(ctx, "CREATE ROLE "+ident(role)+" NOLOGIN NOINHERIT"); err != nil {
		var exists2 bool
		if q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists2) == nil && exists2 {
			return nil // created concurrently
		}
		return fmt.Errorf("create role %s: %w", role, err)
	}
	return nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func withDatabase(base *url.URL, name string, user *url.Userinfo) string {
	u := *base
	u.Path = "/" + name
	if user != nil {
		u.User = user
	}
	return u.String()
}
