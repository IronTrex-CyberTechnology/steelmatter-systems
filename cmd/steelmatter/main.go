// Command steelmatter runs the Steelmatter R0 reference service and its
// database administration tasks.
//
//	steelmatter serve      run the HTTP service and workers (STEELMATTER_DATABASE_URL, STEELMATTER_OBSERVER_URL, STEELMATTER_HTTP_ADDR,
//	                       STEELMATTER_KEYS_DIR, STEELMATTER_PERMIT_TTL_SECONDS, STEELMATTER_WORKER_INTERVAL_MS,
//	                       STEELMATTER_RECOVERY_AGE_SECONDS, STEELMATTER_RATE_LIMIT_PER_MINUTE, STEELMATTER_RATE_LIMIT_BURST)
//	steelmatter principal-add -tenant T -id ID -kind KIND
//	                       create a synthetic principal and print its token once (STEELMATTER_DB_ADMIN_URL)
//	steelmatter migrate    apply migrations (STEELMATTER_DB_ADMIN_URL: owner URL for the target database)
//	steelmatter db-init    create a database, its roles and schema (STEELMATTER_ADMIN_URL: cluster admin URL)
//	steelmatter db-drop -name NAME   drop a db-init database and its roles (STEELMATTER_ADMIN_URL)
//	steelmatter anchor-init          create a local recovery anchor database and its three roles (STEELMATTER_ADMIN_URL)
//	steelmatter anchor-drop -name NAME   drop a local anchor-init database and its roles (STEELMATTER_ADMIN_URL)
//
// serve also reads STEELMATTER_CONNECTOR (none or odoo, with the
// STEELMATTER_ODOO_* settings in connector.go), STEELMATTER_ANCHOR_URL (the
// anchor's engine-writer URL; without it R1 runs with no restore
// protection), STEELMATTER_COMPANY_REF (must equal the Odoo company's
// sm_ref and the module's STEELMATTER_DESTINATION),
// STEELMATTER_REPAIR_REVIEWERS and STEELMATTER_REPAIR_CONFIRMERS
// (comma-separated principal IDs, D5).
//
//	steelmatter version    print the build version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/api"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/db"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/observer"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/server"
)

// version is set at build time by scripts/package.
var version = "dev"

const baseline = "0.6"

// Default per-principal quota. The packet specifies 429 but no number; these
// are local R0 defaults, generous for the demo and tests, pending a team decision.
const (
	defaultRatePerMinute = 1200
	defaultRateBurst     = 200
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "principal-add":
		err = principalAdd(ctx, os.Args[2:])
	case "db-drop":
		err = dbDrop(ctx, os.Args[2:])
	case "db-init":
		err = dbInit(ctx, os.Args[2:])
	case "anchor-init":
		err = anchorInit(ctx, os.Args[2:])
	case "anchor-drop":
		err = anchorDrop(ctx, os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "steelmatter:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: steelmatter serve | migrate | db-init [-name NAME] [-env-out PATH] | anchor-init [-name NAME] [-env-out PATH] | principal-add -tenant T -id ID -kind KIND | version")
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func serve(ctx context.Context) error {
	dsn, err := requireEnv("STEELMATTER_DATABASE_URL")
	if err != nil {
		return err
	}
	addr := os.Getenv("STEELMATTER_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	keysDir := os.Getenv("STEELMATTER_KEYS_DIR")
	if keysDir == "" {
		keysDir = ".local/keys"
	}
	cfg := engine.DefaultConfig()
	cfg.Version = version
	ttl, err := envInt("STEELMATTER_PERMIT_TTL_SECONDS", int(cfg.PermitTTL/time.Second))
	if err != nil {
		return err
	}
	cfg.PermitTTL = time.Duration(ttl) * time.Second
	recovery, err := envInt("STEELMATTER_RECOVERY_AGE_SECONDS", int(cfg.RecoveryAge/time.Second))
	if err != nil {
		return err
	}
	cfg.RecoveryAge = time.Duration(recovery) * time.Second
	interval, err := envInt("STEELMATTER_WORKER_INTERVAL_MS", 1000)
	if err != nil || interval == 0 {
		return fmt.Errorf("STEELMATTER_WORKER_INTERVAL_MS must be a positive integer")
	}

	// Per-principal quota (429). 0 for either value disables it.
	perMinute, err := envInt("STEELMATTER_RATE_LIMIT_PER_MINUTE", defaultRatePerMinute)
	if err != nil {
		return err
	}
	burst, err := envInt("STEELMATTER_RATE_LIMIT_BURST", defaultRateBurst)
	if err != nil {
		return err
	}
	limiter := api.NewLimiter(int64(perMinute), int64(burst))

	trust, signers, err := contract.LoadKeyring(keysDir)
	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}
	pool, err := newPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	defer pool.Close()
	engineSigners, obsSigner, ok := contract.SplitObserver(signers)
	if !ok {
		return errors.New("no signing key for SM-OBSERVATION+JWS")
	}
	eng, err := engine.New(pool, trust, engineSigners, cfg)
	if err != nil {
		return err
	}
	// The observer reads with its own read-only role and signs with the
	// observation key only.
	observerDSN, err := requireEnv("STEELMATTER_OBSERVER_URL")
	if err != nil {
		return err
	}
	observerPool, err := newPool(ctx, observerDSN)
	if err != nil {
		return fmt.Errorf("observer database config: %w", err)
	}
	defer observerPool.Close()
	if err := observer.CheckSeparation(ctx, observerPool, pool); err != nil {
		return fmt.Errorf("observer separation: %w", err)
	}
	eng.Observer = &observer.Observer{Pool: observerPool, Signer: obsSigner, ObserverID: "steelmatter-observer"}
	if company := os.Getenv("STEELMATTER_COMPANY_REF"); company != "" {
		eng.Cfg.CompanyRef = company
	}
	eng.RepairReviewers = splitList(os.Getenv("STEELMATTER_REPAIR_REVIEWERS"))
	eng.RepairConfirmers = splitList(os.Getenv("STEELMATTER_REPAIR_CONFIRMERS"))
	conn, connKind, err := connectorFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	eng.Connector = conn
	eng.Cfg.RequireRecoveryAnchor = conn != nil
	slog.Info("R1 destination connector", "kind", connKind, "company_ref", eng.Cfg.CompanyRef)
	if anchorDSN := os.Getenv("STEELMATTER_ANCHOR_URL"); anchorDSN != "" {
		anchorPool, err := newPool(ctx, anchorDSN)
		if err != nil {
			return fmt.Errorf("anchor database config: %w", err)
		}
		defer anchorPool.Close()
		ws, err := eng.UseAnchor(ctx, &anchor.Client{Pool: anchorPool})
		if err != nil {
			return fmt.Errorf("recovery anchor: %w", err)
		}
		slog.Info("recovery anchor connected", "writes_enabled", ws.Enabled, "generation", ws.Generation, "reason", ws.Reason)
	} else if conn != nil {
		return errors.New("STEELMATTER_ANCHOR_URL is required with an R1 destination connector")
	}
	if conn != nil {
		gate, err := eng.InitializeRecovery(ctx, conn)
		if err != nil {
			return fmt.Errorf("startup recovery barrier: %w", err)
		}
		slog.Info("startup recovery gate", "passed", gate.Passed, "checks", gate.Checks)
	}
	authn := &auth.Authenticator{Pool: pool}

	srv := &http.Server{
		Addr: addr,
		Handler: server.New(server.Info{Version: version, Baseline: baseline}, server.ReadyFunc(eng.Ready), func(mux *http.ServeMux) {
			api.RegisterWith(mux, eng, authn, api.Options{Limiter: limiter})
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      server.RequestTimeout + 5*time.Second, // room to write the envelope after the request deadline
		MaxHeaderBytes:    16 << 10,
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	go eng.RunWorkers(workerCtx, time.Duration(interval)*time.Millisecond, engine.ReceiptSink{E: eng}, func(name string, err error) {
		slog.Warn("worker error", "worker", name, "err", err)
	})

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("steelmatter listening", "addr", addr, "version", version, "failpoints", failpoint.Enabled)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// dbDrop removes a database created by db-init together with its login roles.
func dbDrop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db-drop", flag.ContinueOnError)
	name := fs.String("name", "", "database name to drop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	adminURL, err := requireEnv("STEELMATTER_ADMIN_URL")
	if err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	return db.Drop(ctx, adminURL, &db.Database{Name: *name, ExecutorRole: *name + "_executor", ObserverRole: *name + "_observer"})
}

// anchorInit creates a local recovery anchor (D5 v3, option 1: a separate
// PostgreSQL, tested locally, no paid hosting). Its backups must never be
// restored together with the engine's or Odoo's.
func anchorInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("anchor-init", flag.ContinueOnError)
	name := fs.String("name", "steelmatter_anchor_dev", "anchor database name to create")
	envOut := fs.String("env-out", ".local/anchor.env", "file to write connection URLs to (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	adminURL, err := requireEnv("STEELMATTER_ADMIN_URL")
	if err != nil {
		return err
	}
	if _, err := os.Stat(*envOut); err == nil {
		return fmt.Errorf("%s already exists; remove it and drop the anchor to re-initialise", *envOut)
	}
	d, err := anchor.Provision(ctx, adminURL, *name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*envOut), 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf("# Local recovery anchor %s. Contains generated passwords; never commit. Back it up separately from the\n"+
		"# engine database and Odoo, and never restore it together with them.\n"+
		"STEELMATTER_ANCHOR_URL='%s'\nSTEELMATTER_ANCHOR_DEST_URL='%s'\nSTEELMATTER_ANCHOR_OBSERVER_URL='%s'\nSTEELMATTER_ANCHOR_ADMIN_URL='%s'\n",
		d.Name, d.EngineURL, d.DestURL, d.ObserverURL, d.AdminURL)
	if err := os.WriteFile(*envOut, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Printf("created anchor %s with roles %s, %s and %s; URLs written to %s\n", d.Name, d.EngineRole, d.DestRole, d.ObserverRole, *envOut)
	return nil
}

// anchorDrop removes a local anchor created by anchor-init.
func anchorDrop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("anchor-drop", flag.ContinueOnError)
	name := fs.String("name", "", "anchor database name to drop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	adminURL, err := requireEnv("STEELMATTER_ADMIN_URL")
	if err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	return anchor.Drop(ctx, adminURL, &anchor.Database{Name: *name, EngineRole: *name + "_engine", DestRole: *name + "_dest", ObserverRole: *name + "_observer"})
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newPool opens a pool whose sessions bound every statement, including reads
// outside transactions (F-21).
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30s"
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30s"
	return pgxpool.NewWithConfig(ctx, cfg)
}

func principalAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("principal-add", flag.ContinueOnError)
	tenant := fs.String("tenant", "synthetic-org", "tenant id")
	id := fs.String("id", "", "principal id")
	kind := fs.String("kind", "", "OWNER, ISSUER, ACTOR, OBSERVER or OPERATOR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dsn, err := requireEnv("STEELMATTER_DB_ADMIN_URL")
	if err != nil {
		return err
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ($1, $2, $3, $4)`,
		*id, *tenant, *kind, hash); err != nil {
		return err
	}
	fmt.Println(token) // shown once; only its SHA-256 is stored
	return nil
}

func migrate(ctx context.Context) error {
	dsn, err := requireEnv("STEELMATTER_DB_ADMIN_URL")
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	applied, err := db.Migrate(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Printf("applied %d migration(s)\n", len(applied))
	for _, a := range applied {
		fmt.Println("  ", a)
	}
	return nil
}

func dbInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("db-init", flag.ContinueOnError)
	name := fs.String("name", "steelmatter_dev", "database name to create")
	envOut := fs.String("env-out", ".local/dev.env", "file to write connection URLs to (mode 0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	adminURL, err := requireEnv("STEELMATTER_ADMIN_URL")
	if err != nil {
		return err
	}
	if _, err := os.Stat(*envOut); err == nil {
		return fmt.Errorf("%s already exists; remove it and drop the database to re-initialise", *envOut)
	}
	d, err := db.Provision(ctx, adminURL, *name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*envOut), 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf("# Synthetic development database %s. Contains generated passwords; never commit.\n"+
		"STEELMATTER_DATABASE_URL='%s'\nSTEELMATTER_OBSERVER_URL='%s'\nSTEELMATTER_DB_ADMIN_URL='%s'\n",
		d.Name, d.ExecutorURL, d.ObserverURL, d.AdminURL)
	if err := os.WriteFile(*envOut, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Printf("created database %s with roles %s and %s; URLs written to %s\n", d.Name, d.ExecutorRole, d.ObserverRole, *envOut)
	return nil
}
