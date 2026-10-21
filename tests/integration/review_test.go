//go:build integration

package integration

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// T05 (current entitlement): disabling or changing the trusted assignment
// after a permit is issued stops evaluation and admission; disabling the
// principal stops authentication. No target effect, binding unchanged.
func TestT05RemovedEntitlement(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	asg := fmt.Sprintf("asg-%s-%s", p.mandate, "agent-a")

	e.admin(`UPDATE steelmatter.workflow_assignments SET enabled = false WHERE assignment_id = $1`, asg)
	st, body := e.evaluate("agent-a", p.instruction, 1)
	requireStatus(t, "evaluate after assignment disabled", st, body, 403, "ACTOR_MISMATCH")
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit after assignment disabled", st, body, 403, "ACTOR_MISMATCH")

	// Reassigning the workflow to another actor does not let either actor proceed:
	// the instruction stays bound to agent-a, and agent-a is no longer selected.
	e.admin(`UPDATE steelmatter.workflow_assignments SET enabled = true, actor_id = 'agent-b' WHERE assignment_id = $1`, asg)
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit after reassignment", st, body, 403, "ACTOR_MISMATCH")
	st, body = e.evaluate("agent-b", p.instruction, 1)
	requireStatus(t, "new assignee evaluates", st, body, 403, "ACTOR_MISMATCH")

	// Restoring the assignment restores the actor's entitlement.
	e.admin(`UPDATE steelmatter.workflow_assignments SET actor_id = 'agent-a' WHERE assignment_id = $1`, asg)
	e.admin(`UPDATE steelmatter.principals SET enabled = false WHERE principal_id = 'agent-a'`)
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "disabled principal", st, body, 401, "UNAUTHENTICATED")
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 0 {
		t.Fatal("target effect without current entitlement")
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	var actor string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT actor_id FROM steelmatter.instructions WHERE instruction_id = $1`, p.instruction).Scan(&actor)
	if actor != "agent-a" {
		t.Fatalf("binding changed to %s", actor)
	}

	e.admin(`UPDATE steelmatter.principals SET enabled = true WHERE principal_id = 'agent-a'`)
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "entitlement restored", st, body, 201, "")

	// Identity fields in request bodies are never accepted.
	b := instructionBody("with-actor", p.mandate)
	b["actor_id"] = "agent-b"
	st, body = e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "actor_id in creation body", st, body, 400, "UNKNOWN_FIELD")
	b = instructionBody("with-tenant", p.mandate)
	b["tenant_id"] = "other-org"
	st, body = e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "tenant_id in creation body", st, body, 400, "UNKNOWN_FIELD")
}

// T12: each invalid assignment is rejected for its own reason, with no
// creation and no disclosure of actor identities.
func TestT12AssignmentReasonsAreDistinct(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		setup  func(m string)
		reason string
	}{
		{"missing", func(string) {}, "no actor assignment"},
		{"disabled", func(m string) {
			e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ($1, $2, $3, $4, 'agent-a', false)`, "asg-d-"+m, tenant, source, m)
		}, "disabled"},
		{"non-actor principal assigned", func(m string) {
			e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ($1, $2, $3, $4, 'observer-a', true)`, "asg-o-"+m, tenant, source, m)
		}, "not an enabled actor"},
		{"ambiguous (both actors inside the mandate)", func(m string) {
			e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ($1, $2, $3, $4, 'agent-a', true), ($5, $2, $3, $4, 'agent-b', true)`,
				"asg-a1-"+m, tenant, source, m, "asg-a2-"+m)
		}, "ambiguous"},
		{"actor outside the mandate", func(m string) {
			e.adminBypass(`UPDATE steelmatter.mandates SET actor_ids = '{agent-b}' WHERE mandate_id = $1`, m)
			e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ($1, $2, $3, $4, 'agent-a', true)`, "asg-x-"+m, tenant, source, m)
		}, "outside the mandate"},
		{"disabled principal", func(m string) {
			e.admin(`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, enabled) VALUES ('agent-off', $1, 'ACTOR', false)`, tenant)
			e.adminBypass(`UPDATE steelmatter.mandates SET actor_ids = '{agent-a,agent-b,agent-off}' WHERE mandate_id = $1`, m)
			e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ($1, $2, $3, $4, 'agent-off', true)`, "asg-p-"+m, tenant, source, m)
		}, "not an enabled actor"},
	}
	for i, c := range cases {
		m := e.mandate(mandateOpts{})
		c.setup(m)
		st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody(fmt.Sprintf("r%d", i), m))
		requireStatus(t, c.name, st, body, 422, "ACTOR_ASSIGNMENT_INVALID")
		msg := body["message"].(string)
		if !strings.Contains(msg, c.reason) {
			t.Fatalf("%s: message %q does not state the reason %q", c.name, msg, c.reason)
		}
		if strings.Contains(msg, "agent-") || strings.Contains(msg, "observer-") || len(body) != 4 {
			t.Fatalf("%s: response discloses identities or extra fields: %v", c.name, body)
		}
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions`); n != 0 {
		t.Fatalf("invalid assignments created %d instructions", n)
	}
}

// T22: actor identity has no route to protected writes. Every table in every
// Steelmatter schema refuses writes from a login without grants and from the
// observer; the executor cannot DELETE/TRUNCATE anything or edit protected
// configuration, target orders or supplier data. (Network-level isolation of
// the database is a deployment control, recorded under D05, not tested here.)
func TestT22EveryWritePathDenied(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	ctx := context.Background()

	// Actor tokens on every non-actor endpoint.
	for _, c := range []struct {
		method, path string
		body         any
		status       int
	}{
		{"POST", "/v1/mandates", map[string]any{"actor_ids": []string{"agent-a"}, "audience": audience, "supplier_ids": []string{"supplier-a"},
			"delivery_ids": []string{"warehouse-a"}, "currency": "INR", "limit_minor": 1, "expires_at": "2099-01-01T00:00:00Z"}, 403},
		{"POST", "/v1/mandates/" + p.mandate + "/revoke", nil, 403},
		{"POST", "/v1/instructions", instructionBody("bypass", p.mandate), 403},
		{"POST", "/v1/instructions/" + p.instruction + "/revisions", amendBodyFor(1), 403},
		{"POST", "/v1/operations/" + p.operation + "/reconcile", nil, 404},
	} {
		st, body := e.call(c.method, c.path, "agent-a", c.body)
		if st != c.status {
			t.Fatalf("actor %s %s: %d %v; want %d", c.method, c.path, st, body, c.status)
		}
	}

	tables := e.tables()
	stray := e.strayConn()
	denied := func(who string, conn interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, sql string) {
		t.Helper()
		_, err := conn.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s not denied: %v\n%s", who, err, sql)
		}
	}
	for _, tb := range tables {
		for _, sql := range []string{
			fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, tb.name),
			fmt.Sprintf(`UPDATE %s SET %s = %s`, tb.name, tb.col, tb.col),
			fmt.Sprintf(`DELETE FROM %s`, tb.name),
			fmt.Sprintf(`TRUNCATE %s`, tb.name),
		} {
			denied("login without grants", stray, sql)
			denied("observer", e.h.Observer, sql)
		}
		denied("executor DELETE", e.h.Executor, fmt.Sprintf(`DELETE FROM %s`, tb.name))
		denied("executor TRUNCATE", e.h.Executor, fmt.Sprintf(`TRUNCATE %s`, tb.name))
	}
	for _, sql := range []string{
		`UPDATE target.purchase_orders SET amount_minor = 1`,
		`UPDATE target.purchase_order_lines SET quantity = 1`,
		`UPDATE target.suppliers SET revision = revision + 1`,
		`UPDATE steelmatter.principals SET enabled = true`,
		`UPDATE steelmatter.issuer_entitlements SET source_system = source_system`,
		`UPDATE steelmatter.workflow_assignments SET enabled = true`,
		`INSERT INTO steelmatter.workflow_assignments VALUES ('asg-self', 'synthetic-org', 'synthetic-erp', 'm', 'agent-a', true)`,
		`SELECT * FROM steelmatter_meta.migrations`,
	} {
		denied("executor", e.h.Executor, sql)
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

type table struct{ name, col string }

// tables lists every base table in the Steelmatter schemas with one column.
func (e *env) tables() []table {
	e.t.Helper()
	rows, err := e.h.Admin.Query(context.Background(), `SELECT t.table_schema || '.' || t.table_name,
		(SELECT c.column_name FROM information_schema.columns c WHERE c.table_schema = t.table_schema AND c.table_name = t.table_name ORDER BY c.ordinal_position LIMIT 1)
		FROM information_schema.tables t WHERE t.table_schema IN ('steelmatter', 'steelmatter_meta', 'target') AND t.table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []table
	for rows.Next() {
		var tb table
		if err := rows.Scan(&tb.name, &tb.col); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, tb)
	}
	if len(out) < 15 {
		e.t.Fatalf("only %d tables found", len(out))
	}
	return out
}

// strayConn connects with a login role that has CONNECT but no grants.
func (e *env) strayConn() *pgx.Conn {
	e.t.Helper()
	ctx := context.Background()
	role := e.h.DB.Name + "_stray"
	e.admin(`CREATE ROLE ` + role + ` LOGIN PASSWORD 'stray-synthetic'`)
	e.admin(`GRANT CONNECT ON DATABASE ` + e.h.DB.Name + ` TO ` + role)
	e.t.Cleanup(func() { _, _ = e.h.Admin.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE `+role) })
	cfg, err := pgx.ParseConfig(e.h.DB.ExecutorURL)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg.User, cfg.Password = role, "stray-synthetic"
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func amendBodyFor(expected int64) map[string]any {
	return map[string]any{"amendment_id": "bypass", "expected_action_revision": expected,
		"proposed_action": instructionBody("x", "x")["proposed_action"]}
}

// T23: an actor of another tenant presenting a valid permit learns nothing.
func TestT23CrossTenantAdmitDisclosesNothing(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	st, body := e.admit("agent-x", p.action, p.jws)
	requireStatus(t, "cross-tenant admit", st, body, 404, "NOT_FOUND")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
}

// F-17: signed history cannot be rewritten. The executor has no UPDATE on
// canonical payloads, digests or windows; for every role (administrators
// included) signatures are write-once and a released permit can never be
// reactivated.
func TestF17SignedHistoryImmutable(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	if st, _ := e.reconcile("observer-a", p.operation); st != 200 {
		t.Fatal(st)
	}
	ctx := context.Background()
	expect := func(who string, conn interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, code, sql string, args ...any) {
		t.Helper()
		_, err := conn.Exec(ctx, sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s: got %v; want SQLSTATE %s\n%s", who, err, code, sql)
		}
	}
	// Executor: column privileges.
	for _, sql := range []string{
		`UPDATE steelmatter.mandates SET limit_minor = limit_minor + 1`,
		`UPDATE steelmatter.mandates SET payload_canonical = payload_canonical`,
		`UPDATE steelmatter.action_revisions SET action_canonical = action_canonical`,
		`UPDATE steelmatter.issuances SET permit_canonical = permit_canonical`,
		`UPDATE steelmatter.issuances SET expires_at = expires_at`,
		`UPDATE steelmatter.operations SET execution_canonical = execution_canonical`,
		`UPDATE steelmatter.obligations SET amount_minor = amount_minor`,
		`UPDATE steelmatter.amendments SET request_digest = request_digest`,
		`UPDATE steelmatter.observations SET status = status`,
		`UPDATE steelmatter.instructions SET actor_id = actor_id`,
	} {
		expect("executor", e.h.Executor, "42501", sql)
	}
	// Executor, allowed columns, but the guards refuse.
	expect("executor rewrites execution signature", e.h.Executor, "SM001", `UPDATE steelmatter.operations SET execution_jws = 'x.y.z' WHERE operation_id = $1`, p.operation)
	expect("executor rewrites permit signature", e.h.Executor, "SM001", `UPDATE steelmatter.issuances SET permit_jws = 'x.y.z' WHERE permit_id = $1`, p.permitID)
	expect("executor re-activates a consumed permit", e.h.Executor, "SM001", `UPDATE steelmatter.issuances SET status = 'ACTIVE' WHERE permit_id = $1`, p.permitID)
	// Administrators are bound by the guards too (without disabling triggers).
	expect("admin rewrites mandate payload", e.h.Admin, "SM001", `UPDATE steelmatter.mandates SET mandate_jws = 'x.y.z' WHERE mandate_id = $1`, p.mandate)
	expect("admin rewrites action", e.h.Admin, "SM001", `UPDATE steelmatter.action_revisions SET action_digest = repeat('f', 64) WHERE instruction_id = $1`, p.instruction)

	// A released permit cannot come back.
	q := e.permitted2()
	if _, err := e.eng.Pool.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'EXPIRED', released_at = now() WHERE permit_id = $1`, q); err != nil {
		t.Fatal(err)
	}
	expect("executor reactivates a released permit", e.h.Executor, "SM001", `UPDATE steelmatter.issuances SET status = 'ACTIVE', release_reason = NULL, released_at = NULL WHERE permit_id = $1`, q)
}

// permitted2 issues a second, unrelated permit and returns its permit ID.
func (e *env) permitted2() string {
	e.t.Helper()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("second-record", m)
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(e.t, "second evaluate", st, body, 201, "")
	return body["permit"].(map[string]any)["permit_id"].(string)
}

// F-24: ECDSA signatures are malleable ((r, s) and (r, n-s) both verify).
// Identity is always the payload digest, never the JWS bytes, so a
// re-encoded permit cannot create a second operation or charge.
func TestMalleatedPermitSignatureCreatesNoSecondIdentity(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	parts := strings.Split(p.jws, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatal("unexpected signature encoding")
	}
	n := elliptic.P256().Params().N
	sv := new(big.Int).SetBytes(sig[32:])
	flipped := make([]byte, 64)
	copy(flipped[:32], sig[:32])
	new(big.Int).Sub(n, sv).FillBytes(flipped[32:])
	malleated := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(flipped)
	if malleated == p.jws {
		t.Fatal("malleation produced identical bytes")
	}

	st, first := e.admit("agent-a", p.action, malleated)
	requireStatus(t, "admit with re-encoded signature", st, first, 201, "")
	st, again := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit with original signature", st, again, 200, "")
	if first["operation_id"] != again["operation_id"] || first["operation_id"] != p.operation {
		t.Fatalf("operations %v / %v; want both %s", first["operation_id"], again["operation_id"], p.operation)
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// I02 checklist item 4: malformed amendment requests
// (fixtures/requests/amendment-negative.json) are rejected with their
// specified code, and none changes the instruction, revision, permit or ledger.
func TestAmendmentNegativeRequestFixtures(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	raw, err := os.ReadFile("../../fixtures/requests/amendment-negative.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct {
			Name   string `json:"name"`
			Body   string `json:"body"`
			Status int    `json:"status"`
			Code   string `json:"code"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) < 20 {
		t.Fatalf("only %d fixture cases", len(fx.Cases))
	}
	path := "/v1/instructions/" + p.instruction + "/revisions"
	for _, c := range fx.Cases {
		st, body := e.call("POST", path, "issuer-a", []byte(c.Body))
		requireStatus(t, c.Name, st, body, c.Status, c.Code)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.amendments`); n != 0 {
		t.Fatalf("%d amendments recorded from malformed requests", n)
	}
	if n := e.count(`SELECT current_revision FROM steelmatter.instructions WHERE instruction_id = $1`, p.instruction); n != 1 {
		t.Fatalf("revision moved to %d", n)
	}
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatalf("permit %s after malformed amendments", s)
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	// The valid form of the same request still works.
	st, body := e.call("POST", path, "issuer-a", map[string]any{"amendment_id": "amend-ok", "expected_action_revision": 1,
		"proposed_action": instructionBody("x", "x", line("item-a", 2, 20_000_000))["proposed_action"]})
	requireStatus(t, "valid amendment", st, body, 201, "")
}
