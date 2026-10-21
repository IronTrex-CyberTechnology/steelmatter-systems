//go:build integration

// Package integration runs the R0 acceptance scenarios against the real HTTP
// handlers, engine and PostgreSQL. Each test asserts persisted state, not
// only responses. Tests needing named pause/crash points live in files built
// with the "failpoints" tag.
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/api"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/auth"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/dbtest"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/engine"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/observer"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/server"
)

const (
	tenant   = "synthetic-org"
	source   = "synthetic-erp"
	limit    = int64(100_000_000) // INR 10,00,000
	sixLakh  = int64(60_000_000)  // INR 6,00,000
	audience = "synthetic-procurement"
)

type env struct {
	t       *testing.T
	h       *dbtest.Harness
	eng     *engine.Engine
	srv     *httptest.Server
	url     string // base URL requests go to (the in-process server unless a test points it at a subprocess)
	keysDir string
	tokens  map[string]string
}

type envOpts struct {
	permitTTL time.Duration
	limiter   *api.Limiter // per-principal quota; nil = none
}

// newEnv provisions a database, synthetic keys, principals and suppliers and
// serves the real API over httptest.
func newEnv(t *testing.T, o ...envOpts) *env {
	t.Helper()
	opts := envOpts{permitTTL: 5 * time.Minute}
	if len(o) > 0 && o[0].permitTTL > 0 {
		opts.permitTTL = o[0].permitTTL
	}
	if len(o) > 0 {
		opts.limiter = o[0].limiter
	}
	h := dbtest.New(t)
	e := &env{t: t, h: h, keysDir: t.TempDir(), tokens: map[string]string{}}
	now := time.Now()
	for name, purposes := range map[string][]string{
		"steelmatter-service":  {"SM-PERMIT+JWS", "SM-EXECUTION+JWS"},
		"synthetic-owner":      {"SM-MANDATE+JWS"},
		"steelmatter-observer": {"SM-OBSERVATION+JWS"},
	} {
		if _, err := keys.Provision(e.keysDir, name, purposes, now); err != nil {
			t.Fatal(err)
		}
	}
	trust, signers, err := contract.LoadKeyring(e.keysDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.DefaultConfig()
	cfg.PermitTTL = opts.permitTTL
	cfg.RecoveryAge = 0
	engineSigners, obsSigner, ok := contract.SplitObserver(signers)
	if !ok {
		t.Fatal("no observation signer")
	}
	e.eng, err = engine.New(h.Executor, trust, engineSigners, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.CheckSeparation(context.Background(), h.Observer, h.Executor); err != nil {
		t.Fatal(err)
	}
	e.eng.Observer = &observer.Observer{Pool: h.Observer, Signer: obsSigner, ObserverID: "steelmatter-observer"}
	handler := server.New(server.Info{Version: "test", Baseline: "0.6"}, server.ReadyFunc(e.eng.Ready), func(mux *http.ServeMux) {
		api.RegisterWith(mux, e.eng, &auth.Authenticator{Pool: h.Executor}, api.Options{Limiter: opts.limiter})
	})
	e.srv = httptest.NewServer(handler)
	e.url = e.srv.URL
	t.Cleanup(e.srv.Close)

	for _, p := range []struct{ id, tenant, kind string }{
		{"owner-a", tenant, "OWNER"}, {"issuer-a", tenant, "ISSUER"}, {"issuer-b", tenant, "ISSUER"},
		{"agent-a", tenant, "ACTOR"}, {"agent-b", tenant, "ACTOR"}, {"observer-a", tenant, "OBSERVER"}, {"operator-a", tenant, "OPERATOR"},
		{"owner-x", "other-org", "OWNER"}, {"issuer-x", "other-org", "ISSUER"}, {"agent-x", "other-org", "ACTOR"},
	} {
		token, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		e.admin(`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ($1, $2, $3, $4)`, p.id, p.tenant, p.kind, hash)
		e.tokens[p.id] = token
	}
	e.admin(`INSERT INTO steelmatter.issuer_entitlements (tenant_id, issuer_principal, source_system) VALUES ($1, 'issuer-a', $2), ('other-org', 'issuer-x', $2)`, tenant, source)
	e.admin(`INSERT INTO steelmatter.issuer_entitlements (tenant_id, issuer_principal, source_system) VALUES ($1, 'issuer-a', 'synthetic-erp-b')`, tenant)
	e.admin(`INSERT INTO target.suppliers (tenant_id, supplier_id, revision, status) VALUES ($1, 'supplier-a', 1, 'ACTIVE'), ($1, 'supplier-b', 1, 'ACTIVE')`, tenant)
	return e
}

func (e *env) admin(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.h.Admin.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("admin SQL: %v\n%s", err, sql)
	}
}

// adminBypass runs SQL as a database administrator who disables triggers
// (session_replication_role = replica). It models a compromised or careless
// administrator tampering with stored records; the immutability triggers
// otherwise refuse such changes for every role.
func (e *env) adminBypass(sql string, args ...any) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.h.Admin.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		e.t.Fatalf("admin bypass SQL: %v\n%s", err, sql)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.h.Admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// call sends a request as principal who ("" = no credential). body may be
// []byte (sent raw) or any value (JSON-encoded).
func (e *env) call(method, path, who string, body any) (int, map[string]any) {
	e.t.Helper()
	status, raw := e.callRaw(method, path, who, body)
	var m map[string]any
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			e.t.Fatalf("%s %s: non-JSON response %d: %s", method, path, status, raw)
		}
	}
	return status, m
}

func (e *env) callRaw(method, path, who string, body any) (int, []byte) {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.url+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil // transport failure (e.g. connection dropped after commit)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func requireStatus(t *testing.T, what string, got int, body map[string]any, want int, code string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d (%v); want %d %s", what, got, body, want, code)
	}
	if code != "" {
		c := body["error_code"]
		if c == nil {
			c = body["reason"]
		}
		if c != code {
			t.Fatalf("%s: code %v (%v); want %s", what, c, body["message"], code)
		}
	}
}

// ---- scenario building blocks ----

type mandateOpts struct {
	limit     int64
	audience  string
	actors    []string
	suppliers []string
	expiresIn time.Duration
}

func (e *env) mandate(o mandateOpts) string {
	e.t.Helper()
	if o.limit == 0 {
		o.limit = limit
	}
	if o.audience == "" {
		o.audience = audience
	}
	if o.actors == nil {
		o.actors = []string{"agent-a", "agent-b"}
	}
	if o.suppliers == nil {
		o.suppliers = []string{"supplier-a", "supplier-b"}
	}
	if o.expiresIn == 0 {
		o.expiresIn = time.Hour
	}
	st, body := e.call("POST", "/v1/mandates", "owner-a", map[string]any{
		"actor_ids": o.actors, "audience": o.audience, "supplier_ids": o.suppliers, "delivery_ids": []string{"warehouse-a"},
		"currency": "INR", "limit_minor": o.limit, "expires_at": time.Now().UTC().Add(o.expiresIn).Format("2006-01-02T15:04:05Z"),
	})
	requireStatus(e.t, "create mandate", st, body, 201, "")
	m := body["mandate"].(map[string]any)
	return m["mandate_id"].(string)
}

// assign maps (source, mandate) to actor in the trusted workflow table.
func (e *env) assign(mandateID, actor string) {
	e.t.Helper()
	e.admin(`INSERT INTO steelmatter.workflow_assignments (assignment_id, tenant_id, source_system, mandate_id, actor_id, enabled)
		VALUES ($1, $2, $3, $4, $5, true)`, fmt.Sprintf("asg-%s-%s", mandateID, actor), tenant, source, mandateID, actor)
}

func line(item string, qty, price int64) map[string]any {
	return map[string]any{"item_id": item, "quantity": qty, "unit_price_minor": price}
}

func instructionBody(record, mandateID string, lines ...map[string]any) map[string]any {
	if len(lines) == 0 {
		lines = []map[string]any{line("item-a", 3, 20_000_000)} // INR 6,00,000
	}
	return map[string]any{"source_system": source, "source_record_id": record, "mandate_id": mandateID,
		"proposed_action": map[string]any{"supplier_id": "supplier-a", "delivery_id": "warehouse-a", "currency": "INR", "lines": lines}}
}

func (e *env) instruction(record, mandateID string, lines ...map[string]any) map[string]any {
	e.t.Helper()
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody(record, mandateID, lines...))
	requireStatus(e.t, "create instruction", st, body, 201, "")
	return body
}

func (e *env) evaluate(who, instructionID string, rev int64) (int, map[string]any) {
	e.t.Helper()
	return e.call("POST", "/v1/actions/evaluate", who, map[string]any{"instruction_id": instructionID, "action_revision": rev})
}

func (e *env) admit(who string, action any, jws string) (int, map[string]any) {
	e.t.Helper()
	return e.call("POST", "/v1/operations", who, map[string]any{"action": action, "permit_jws": jws})
}

// permitted is a ready-to-admit scenario: mandate (limit 100,000,000),
// assignment to agent-a, one 60,000,000 instruction and an issued permit.
type permitted struct {
	mandate, instruction string
	action               map[string]any
	jws                  string
	permitID, operation  string
}

func (e *env) permitted() permitted {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(e.t, "evaluate", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	return permitted{mandate: m, instruction: inst["instruction_id"].(string), action: body["action"].(map[string]any),
		jws: body["permit_jws"].(string), permitID: permit["permit_id"].(string), operation: permit["operation_id"].(string)}
}

// ledger is the persisted state most scenarios assert.
type ledger struct {
	Available, Reserved, Committed, Unresolved int64
	Orders, Obligations, Operations, Outbox    int
}

func (e *env) ledger(mandateID string) ledger {
	e.t.Helper()
	var l ledger
	var lim int64
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT available_minor, reserved_minor, committed_minor, unresolved_minor, limit_minor
		FROM steelmatter.mandates WHERE mandate_id = $1`, mandateID).Scan(&l.Available, &l.Reserved, &l.Committed, &l.Unresolved, &lim); err != nil {
		e.t.Fatal(err)
	}
	if l.Available+l.Reserved+l.Committed+l.Unresolved != lim || l.Available < 0 || l.Reserved < 0 || l.Committed < 0 || l.Unresolved < 0 {
		e.t.Fatalf("I3 violated: %+v (limit %d)", l, lim)
	}
	l.Orders = e.count(`SELECT count(*) FROM target.purchase_orders`)
	l.Obligations = e.count(`SELECT count(*) FROM steelmatter.obligations`)
	l.Operations = e.count(`SELECT count(*) FROM steelmatter.operations`)
	l.Outbox = e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'operation.committed'`)
	return l
}

func requireLedger(t *testing.T, got, want ledger) {
	t.Helper()
	if got != want {
		t.Fatalf("ledger\n got %+v\nwant %+v", got, want)
	}
}

func (e *env) issuanceStatus(permitID string) (status, reason string) {
	e.t.Helper()
	var r *string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT status, release_reason FROM steelmatter.issuances WHERE permit_id = $1`, permitID).Scan(&status, &r); err != nil {
		e.t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	return
}

// committedLedger is the state after one successful 60,000,000 admission.
var committedLedger = ledger{Available: limit - sixLakh, Committed: sixLakh, Orders: 1, Obligations: 1, Operations: 1, Outbox: 1}

// reservedLedger is the state with one active 60,000,000 reservation.
var reservedLedger = ledger{Available: limit - sixLakh, Reserved: sixLakh}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var c map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&c)
	return c
}

func jwsPayload(t *testing.T, jws string) map[string]any {
	t.Helper()
	parts := bytes.Split([]byte(jws), []byte("."))
	raw, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// waitFor polls cond until it holds, failing after 15 s (no unbounded loops).
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

// waitExpired waits until the database clock passes every issuance expiry
// matching the optional permit ID.
func (e *env) waitExpired(permitID string) {
	e.t.Helper()
	e.waitFor("permit expiry", func() bool {
		if permitID == "" {
			return e.count(`SELECT count(*) FROM steelmatter.issuances WHERE clock_timestamp() >= expires_at`) > 0
		}
		return e.count(`SELECT count(*) FROM steelmatter.issuances WHERE permit_id = $1 AND clock_timestamp() >= expires_at`, permitID) == 1
	})
}
