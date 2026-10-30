// Command smdemo runs the R0 CLI demonstration against a running service.
// Simulation with synthetic data only.
//
//	smdemo setup -admin-url URL -tokens FILE      create synthetic principals, entitlements and suppliers
//	smdemo run   -server URL -admin-url URL -tokens FILE [-scenario all|flagship|stale-supplier|lost-response|agent-injection]
//	             [-out DIR] [-json FILE] [-commit SHA] [-reference FILE]
//	                                               exit 1 if any scenario fails or results drift from the
//	                                               pinned reference (T43; "" skips the comparison)
//	smdemo reset -admin-url URL                    return to the synthetic starting state (keeps principals)
//
// -admin-url defaults to STEELMATTER_DB_ADMIN_URL (written by `steelmatter db-init`).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/examples/procurement"
)

const tenant = "synthetic-org"

var principals = []struct{ id, kind string }{
	{"owner-a", auth.Owner}, {"issuer-a", auth.Issuer}, {"agent-a", auth.Actor}, {"agent-b", auth.Actor},
	{"observer-a", auth.Observer}, {"operator-a", auth.Operator},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: smdemo setup|run|reset [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	adminURL := fs.String("admin-url", os.Getenv("STEELMATTER_DB_ADMIN_URL"), "database administrator URL")
	tokens := fs.String("tokens", ".local/demo/tokens.json", "synthetic principal tokens file (mode 0600)")
	server := fs.String("server", envOr("STEELMATTER_SERVER", "http://127.0.0.1:8080"), "service base URL")
	scenario := fs.String("scenario", "all", "scenario name or all")
	outDir := fs.String("out", "", "directory for the flagship evidence bundle")
	jsonOut := fs.String("json", "", "write scenario results as JSON")
	commit := fs.String("commit", "unknown", "source commit recorded in the results trace")
	refPath := fs.String("reference", procurement.ReferencePath, "pinned demo reference to compare with (\"\" to skip)")
	_ = fs.Parse(os.Args[2:])
	if *adminURL == "" {
		fail("-admin-url or STEELMATTER_DB_ADMIN_URL is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *adminURL)
	if err != nil {
		fail("database: %v", err)
	}
	defer pool.Close()

	switch os.Args[1] {
	case "setup":
		err = setup(ctx, pool, *tokens)
	case "reset":
		err = reset(ctx, pool)
	case "run":
		os.Exit(run(ctx, pool, *server, *tokens, *scenario, *outDir, *jsonOut, *commit, *refPath))
	default:
		fail("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail("%v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "smdemo: "+format+"\n", a...)
	os.Exit(2)
}

// setup creates (or rotates the tokens of) the synthetic principals and the
// fixed synthetic configuration. Tokens are written once to a 0600 file.
func setup(ctx context.Context, pool *pgxpool.Pool, tokensPath string) error {
	toks := map[string]string{}
	for _, p := range principals {
		token, hash, err := auth.NewToken()
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ($1, $2, $3, $4)
			ON CONFLICT (principal_id) DO UPDATE SET token_sha256 = EXCLUDED.token_sha256, enabled = true`, p.id, tenant, p.kind, hash); err != nil {
			return err
		}
		toks[p.id] = token
	}
	for _, s := range []string{procurement.SourceA, procurement.SourceB} {
		if _, err := pool.Exec(ctx, `INSERT INTO steelmatter.issuer_entitlements VALUES ($1, 'issuer-a', $2) ON CONFLICT DO NOTHING`, tenant, s); err != nil {
			return err
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status) VALUES ($1, 'supplier-a', 1, 'ACTIVE'), ($1, 'supplier-b', 1, 'ACTIVE')
		ON CONFLICT DO NOTHING`, tenant); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokensPath), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(toks, "", "  ")
	if err := os.WriteFile(tokensPath, b, 0o600); err != nil {
		return err
	}
	fmt.Printf("synthetic principals ready (%d); tokens written to %s (mode 0600)\n", len(principals), tokensPath)
	return nil
}

// reset returns scenario state to the synthetic starting point.
func reset(ctx context.Context, pool *pgxpool.Pool) error {
	// R1 tables (dispatches, R1 evidence, approvals, worker backoff) refer to
	// the R0 rows, so they are truncated in the same statement
	// (NEW-P6-SDET-01). Trusted configuration (principals, entitlements,
	// approval policies) is kept.
	_, err := pool.Exec(ctx, `TRUNCATE target.payment_requests, target.invoice_references, steelmatter.observations, steelmatter.operations, steelmatter.obligations, steelmatter.outbox,
		steelmatter.event_receipts, steelmatter.amendments, steelmatter.issuances, steelmatter.action_revisions, steelmatter.instructions,
		steelmatter.workflow_assignments, steelmatter.mandates, target.purchase_order_lines, target.purchase_orders,
		steelmatter.r1_observations, steelmatter.r1_executions, steelmatter.dispatches, steelmatter.approvals, steelmatter.worker_backoff;
		UPDATE target.suppliers SET revision = 1, status = 'ACTIVE'`)
	if err == nil {
		fmt.Println("demo state reset to the synthetic starting point")
	}
	return err
}

func run(ctx context.Context, pool *pgxpool.Pool, server, tokensPath, scenario, outDir, jsonOut, commit, refPath string) int {
	b, err := os.ReadFile(tokensPath)
	if err != nil {
		fail("tokens: %v (run `smdemo setup` first)", err)
	}
	var toks map[string]string
	if err := json.Unmarshal(b, &toks); err != nil {
		fail("tokens: %v", err)
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			fail("%v", err)
		}
	}
	env := &procurement.Env{BaseURL: strings.TrimRight(server, "/"), Tokens: toks, Admin: pool, Tenant: tenant,
		RunID: fmt.Sprint(time.Now().UnixNano()), Out: os.Stdout, EvidenceDir: outDir}
	fmt.Printf("Steelmatter procurement reference — CLI demonstration\nserver %s · commit %s · Simulation · Synthetic data\n", env.BaseURL, commit)
	results := procurement.RunAll(ctx, env, scenario)
	if len(results) == 0 {
		fail("unknown scenario %q", scenario)
	}
	failed := 0
	fmt.Println("\nsummary:")
	for _, r := range results {
		fmt.Printf("  %-16s %-8s %s\n", r.Scenario, r.Status, r.Case)
		if r.Status != "PASS" {
			failed++
		}
	}
	refReport := map[string]any{"path": refPath, "checked": false}
	if refPath != "" {
		diffs, err := compareReference(refPath, results)
		if err != nil {
			diffs = []string{err.Error()}
		}
		refReport["checked"], refReport["consistent"], refReport["mismatches"] = true, len(diffs) == 0, diffs
		if len(diffs) > 0 {
			fmt.Printf("\nresults drift from the pinned reference %s:\n  %s\n", refPath, strings.Join(diffs, "\n  "))
			failed++
		} else {
			fmt.Printf("\nresults match the pinned reference %s\n", refPath)
		}
	}
	if jsonOut != "" {
		jb, _ := json.MarshalIndent(map[string]any{"server": env.BaseURL, "commit": commit, "simulation": true, "synthetic": true,
			"reference": refReport, "results": results}, "", "  ")
		if err := os.WriteFile(jsonOut, jb, 0o644); err != nil {
			fail("%v", err)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// compareReference checks the run against the pinned reference (T43). For a
// single-scenario run only that scenario is compared.
func compareReference(path string, results []procurement.Result) ([]string, error) {
	ref, err := procurement.LoadReference(path)
	if err != nil {
		return nil, err
	}
	ran := map[string]bool{}
	for _, r := range results {
		ran[r.Scenario] = true
	}
	kept := ref.Scenarios[:0:0]
	for _, s := range ref.Scenarios {
		if ran[s.Scenario] {
			kept = append(kept, s)
		}
	}
	ref.Scenarios = kept
	return ref.Check(results, "."), nil
}
