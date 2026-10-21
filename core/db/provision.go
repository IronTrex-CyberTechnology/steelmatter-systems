package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// Database describes a provisioned Steelmatter database and its login roles.
// The URLs embed generated passwords; never log them.
type Database struct {
	Name         string
	AdminURL     string
	ExecutorRole string
	ExecutorURL  string
	ObserverRole string
	ObserverURL  string
}

var dbNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Provision creates database name, its executor/observer login roles and
// applies all migrations. adminURL must be a postgres:// URL for a role with
// CREATEDB and CREATEROLE. CONNECT is revoked from PUBLIC so only this
// database's own login roles (and the administrator) can connect.
func Provision(ctx context.Context, adminURL, name string) (*Database, error) {
	return ProvisionUpTo(ctx, adminURL, name, 0)
}

// ProvisionUpTo is Provision with migrations applied only up to version upTo
// (0 = all); used to test upgrades of existing databases.
func ProvisionUpTo(ctx context.Context, adminURL, name string, upTo int) (*Database, error) {
	if !dbNamePattern.MatchString(name) {
		return nil, fmt.Errorf("database name %q must match %s", name, dbNamePattern)
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

	if err := EnsureGroupRoles(ctx, admin); err != nil {
		return nil, err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident(name)); err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}

	d := &Database{Name: name, ExecutorRole: name + "_executor", ObserverRole: name + "_observer"}
	cleanup := func() { _ = Drop(context.WithoutCancel(ctx), adminURL, d) }

	execPW, err := RandomPassword()
	if err != nil {
		cleanup()
		return nil, err
	}
	obsPW, err := RandomPassword()
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := CreateLoginRole(ctx, admin, d.ExecutorRole, execPW, GroupExecutor); err != nil {
		cleanup()
		return nil, err
	}
	if err := CreateLoginRole(ctx, admin, d.ObserverRole, obsPW, GroupObserver); err != nil {
		cleanup()
		return nil, err
	}
	stmts := []string{
		"REVOKE CONNECT, TEMPORARY ON DATABASE " + ident(name) + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + ident(name) + " TO " + ident(d.ExecutorRole) + ", " + ident(d.ObserverRole),
	}
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			cleanup()
			return nil, fmt.Errorf("database privileges: %w", err)
		}
	}

	d.AdminURL = withDatabase(base, name, nil)
	d.ExecutorURL = withDatabase(base, name, url.UserPassword(d.ExecutorRole, execPW))
	d.ObserverURL = withDatabase(base, name, url.UserPassword(d.ObserverRole, obsPW))

	target, err := pgx.Connect(ctx, d.AdminURL)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("connect new database: %w", err)
	}
	defer target.Close(ctx)
	if _, err := MigrateUpTo(ctx, target, upTo); err != nil {
		cleanup()
		return nil, err
	}
	return d, nil
}

// Drop removes the database and its login roles. Missing objects are ignored.
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
	for _, r := range []string{d.ExecutorRole, d.ObserverRole} {
		if r == "" {
			continue
		}
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+ident(r)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// withDatabase copies base, replacing the database path and, when user is
// non-nil, the credentials.
func withDatabase(base *url.URL, name string, user *url.Userinfo) string {
	u := *base
	u.Path = "/" + name
	if user != nil {
		u.User = user
	}
	return u.String()
}
