//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Flagship path: mandate, instruction, permit, admission, replay.
func TestFlagshipCommitAndReplay(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	requireLedger(t, e.ledger(p.mandate), reservedLedger)

	st, first := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit", st, first, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
	if s, _ := e.issuanceStatus(p.permitID); s != "CONSUMED" {
		t.Fatalf("issuance %s", s)
	}
	st, op := e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "lookup", st, op, 200, "")
	if op["execution"] != "COMMITTED" || op["authorisation"] != "PERMITTED" || op["evidence"] != "AVAILABLE" || op["observation"] != "NOT_OBSERVED" {
		t.Fatalf("status %v", op)
	}
}

// T09: identical operation repeated → one order, one charge, original result.
func TestT09Replay(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	st, first := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "first", st, first, 201, "")
	for i := 0; i < 3; i++ {
		st, again := e.admit("agent-a", p.action, p.jws)
		requireStatus(t, "replay", st, again, 200, "")
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("replay result differs:\n%s\n%s", a, b)
		}
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// T09 after expiry: the stored result is returned even though the permit has expired.
func TestT09ReplayAfterPermitExpiry(t *testing.T) {
	e := newEnv(t, envOpts{permitTTL: 2 * time.Second})
	p := e.permitted()
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit", st, body, 201, "")
	time.Sleep(2500 * time.Millisecond) // wall-clock expiry of the historical permit
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "replay after expiry", st, body, 200, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// T03: change amount, supplier, line item or delivery after the permit.
func TestT03MaterialTamper(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	mutations := map[string]func(a map[string]any){
		"amount": func(a map[string]any) {
			a["lines"] = []any{line("item-a", 3, 20_000_001)}
			a["amount_minor"] = 60_000_003
		},
		"supplier": func(a map[string]any) { a["supplier_id"] = "supplier-b" },
		"line item": func(a map[string]any) {
			a["lines"] = []any{line("item-z", 3, 20_000_000)}
		},
		"delivery": func(a map[string]any) { a["delivery_id"] = "warehouse-b" },
	}
	for name, mutate := range mutations {
		a := clone(p.action)
		mutate(a)
		st, body := e.admit("agent-a", a, p.jws)
		requireStatus(t, name, st, body, 409, "ACTION_CHANGED")
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatalf("tampering changed the issuance: %s", s)
	}
}

// T04: protected supplier revision changes before admission.
func TestT04StaleSupplier(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	e.admin(`UPDATE target.suppliers SET revision = 2 WHERE supplier_id = 'supplier-a'`)

	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit stale", st, body, 409, "SUPPLIER_REVISION_STALE")
	st, body = e.evaluate("agent-a", p.instruction, 1)
	requireStatus(t, "re-evaluate stale revision", st, body, 409, "SUPPLIER_REVISION_STALE")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)

	// Reauthorisation: amend (binds supplier revision 2), evaluate, admit.
	st, body = e.call("POST", "/v1/instructions/"+p.instruction+"/revisions", "issuer-a", map[string]any{
		"amendment_id": "rebind-supplier", "expected_action_revision": 1,
		"proposed_action": instructionBody("x", "x")["proposed_action"]})
	requireStatus(t, "amend", st, body, 201, "")
	if body["action"].(map[string]any)["supplier_revision"] != json.Number("2") {
		t.Fatalf("new revision binds supplier revision %v", body["action"])
	}
	st, body = e.evaluate("agent-a", p.instruction, 2)
	requireStatus(t, "evaluate revision 2", st, body, 201, "")
	st, res := e.admit("agent-a", body["action"], body["permit_jws"].(string))
	requireStatus(t, "admit revision 2", st, res, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// T05: the server stores the mapped actor; only that actor can evaluate/admit.
func TestT05ActorSubstitution(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	if inst["actor_id"] != "agent-a" {
		t.Fatalf("stored actor %v; want the mapped actor, not the issuer", inst["actor_id"])
	}
	id := inst["instruction_id"].(string)

	st, body := e.evaluate("issuer-a", id, 1)
	requireStatus(t, "issuer evaluates", st, body, 403, "FORBIDDEN")
	st, body = e.evaluate("agent-b", id, 1)
	requireStatus(t, "other actor evaluates", st, body, 403, "ACTOR_MISMATCH")
	st, body = e.call("POST", "/v1/actions/evaluate", "agent-a", map[string]any{"instruction_id": id, "action_revision": 1, "actor_id": "agent-b"})
	requireStatus(t, "conflicting body identity", st, body, 400, "UNKNOWN_FIELD")
	if n := e.count(`SELECT count(*) FROM steelmatter.issuances`); n != 0 {
		t.Fatalf("substitution attempts created %d issuances", n)
	}

	st, ev := e.evaluate("agent-a", id, 1)
	requireStatus(t, "assigned actor evaluates", st, ev, 201, "")
	st, body = e.admit("agent-b", ev["action"], ev["permit_jws"].(string))
	requireStatus(t, "other actor admits", st, body, 403, "ACTOR_MISMATCH")
	st, body = e.admit("issuer-a", ev["action"], ev["permit_jws"].(string))
	requireStatus(t, "issuer admits", st, body, 403, "FORBIDDEN")
	a := clone(ev["action"].(map[string]any))
	a["actor_id"] = "agent-b"
	st, body = e.admit("agent-b", a, ev["permit_jws"].(string))
	requireStatus(t, "conflicting action identity", st, body, 403, "ACTOR_MISMATCH")

	var actor string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT actor_id FROM steelmatter.instructions WHERE instruction_id = $1`, id).Scan(&actor); err != nil || actor != "agent-a" {
		t.Fatalf("binding changed to %q", actor)
	}
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 0 {
		t.Fatal("substitution created a target effect")
	}
}

// T06: permit presented to a different destination.
func TestT06AudienceSubstitution(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{audience: "other-destination"})
	e.assign(m, "agent-a")
	inst := e.instruction("po-request-001", m)
	st, ev := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate", st, ev, 201, "")
	st, body := e.admit("agent-a", ev["action"], ev["permit_jws"].(string))
	requireStatus(t, "admit at wrong destination", st, body, 403, "AUDIENCE_MISMATCH")
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 0 {
		t.Fatal("target mutated")
	}
}

// T11: rerolled client request IDs resolve the original instruction.
func TestT11IDReroll(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	b := instructionBody("po-request-001", m)
	b["client_request_id"] = "client-1"
	st, first := e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "create", st, first, 201, "")
	for _, cid := range []string{"client-2", "client-3"} {
		b["client_request_id"] = cid
		st, again := e.call("POST", "/v1/instructions", "issuer-a", b)
		requireStatus(t, "reroll "+cid, st, again, 200, "")
		if again["instruction_id"] != first["instruction_id"] {
			t.Fatal("reroll created a new instruction")
		}
	}
	delete(b, "client_request_id")
	st, again := e.call("POST", "/v1/instructions", "issuer-a", b)
	requireStatus(t, "without client id", st, again, 200, "")
	id := first["instruction_id"].(string)
	st, ev1 := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate", st, ev1, 201, "")
	st, ev2 := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate retry", st, ev2, 200, "")
	if ev1["permit_jws"] != ev2["permit_jws"] {
		t.Fatal("retry returned a different permit")
	}
	st, r1 := e.admit("agent-a", ev1["action"], ev1["permit_jws"].(string))
	requireStatus(t, "admit", st, r1, 201, "")
	st, r2 := e.admit("agent-a", ev1["action"], ev1["permit_jws"].(string))
	requireStatus(t, "admit retry", st, r2, 200, "")
	if e.count(`SELECT count(*) FROM steelmatter.instructions`) != 1 || e.count(`SELECT count(*) FROM steelmatter.issuances`) != 1 {
		t.Fatal("rerolled IDs created extra state")
	}
	requireLedger(t, e.ledger(m), committedLedger)
}

// T12: forged source and invalid actor assignments.
func TestT12ForgedSource(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{actors: []string{"agent-a"}})

	st, body := e.call("POST", "/v1/instructions", "agent-a", instructionBody("r1", m))
	requireStatus(t, "agent creates instruction", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/instructions", "issuer-b", instructionBody("r1", m))
	requireStatus(t, "unentitled issuer", st, body, 403, "FORBIDDEN")
	other := instructionBody("r1", m)
	other["source_system"] = "other-erp"
	st, body = e.call("POST", "/v1/instructions", "issuer-a", other)
	requireStatus(t, "source-system impersonation", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/instructions", "issuer-x", instructionBody("r1", m))
	requireStatus(t, "other tenant issuer", st, body, 404, "NOT_FOUND")

	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("r1", m))
	requireStatus(t, "missing assignment", st, body, 422, "ACTOR_ASSIGNMENT_INVALID")
	e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ('asg-1', $1, $2, $3, 'agent-a', false)`, tenant, source, m)
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("r1", m))
	requireStatus(t, "disabled assignment", st, body, 422, "ACTOR_ASSIGNMENT_INVALID")
	e.admin(`INSERT INTO steelmatter.workflow_assignments VALUES ('asg-2', $1, $2, $3, 'agent-b', true)`, tenant, source, m)
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("r1", m))
	requireStatus(t, "out-of-mandate assignment", st, body, 422, "ACTOR_ASSIGNMENT_INVALID")
	e.admin(`UPDATE steelmatter.workflow_assignments SET enabled = true WHERE assignment_id = 'asg-1'`)
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("r1", m))
	requireStatus(t, "ambiguous assignment", st, body, 422, "ACTOR_ASSIGNMENT_INVALID")

	if n := e.count(`SELECT count(*) FROM steelmatter.instructions`); n != 0 {
		t.Fatalf("forged or invalid requests created %d instructions", n)
	}
	// Amendment by an agent is also rejected.
	e.admin(`DELETE FROM steelmatter.workflow_assignments WHERE assignment_id = 'asg-2'`)
	inst := e.instruction("r1", m)
	st, body = e.call("POST", "/v1/instructions/"+inst["instruction_id"].(string)+"/revisions", "agent-a", map[string]any{
		"amendment_id": "a1", "expected_action_revision": 1, "proposed_action": instructionBody("x", "x")["proposed_action"]})
	requireStatus(t, "agent amends", st, body, 403, "FORBIDDEN")
}

// T20: numeric/schema abuse is rejected before signature, capacity or effect.
func TestT20NumericSchemaAbuse(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	before := e.ledger(p.mandate)
	good := instructionBody("abuse", p.mandate)
	raw, _ := json.Marshal(good)
	cases := []struct {
		name string
		body []byte
		code string
	}{
		{"negative quantity", []byte(strings.Replace(string(raw), `"quantity":3`, `"quantity":-3`, 1)), "SCHEMA_VIOLATION"},
		{"float price", []byte(strings.Replace(string(raw), `"unit_price_minor":20000000`, `"unit_price_minor":20000000.5`, 1)), "NON_INTEGER_NUMBER"},
		{"overflow", []byte(strings.Replace(string(raw), `"quantity":3`, `"quantity":9007199254740992`, 1)), "NUMBER_OUT_OF_RANGE"},
		{"line total overflow", []byte(strings.Replace(string(raw), `"quantity":3`, `"quantity":9007199254740991`, 1)), "ARITHMETIC_OVERFLOW"},
		{"duplicate key", []byte(`{"mandate_id":"x",` + string(raw)[1:]), "DUPLICATE_KEY"},
		{"extra field", []byte(`{"amount_minor":1,` + string(raw)[1:]), "UNKNOWN_FIELD"},
		{"unsupported currency", []byte(strings.Replace(string(raw), `"currency":"INR"`, `"currency":"USD"`, 1)), "UNSUPPORTED_CURRENCY"},
	}
	for _, c := range cases {
		st, body := e.call("POST", "/v1/instructions", "issuer-a", c.body)
		want := 400
		if c.code == "UNSUPPORTED_CURRENCY" {
			want = 422
		}
		requireStatus(t, c.name, st, body, want, c.code)
	}
	// Admission body abuse must fail before any lock or capacity change.
	act, _ := json.Marshal(p.action)
	for name, c := range map[string]struct{ body, code string }{
		"float amount": {strings.Replace(string(act), `"amount_minor":60000000`, `"amount_minor":60000000.0`, 1), "NON_INTEGER_NUMBER"},
		"dup key":      {`{"amount_minor":60000000,` + string(act)[1:], "DUPLICATE_KEY"},
		"overflow":     {strings.Replace(string(act), `"amount_minor":60000000`, `"amount_minor":9007199254740992`, 1), "NUMBER_OUT_OF_RANGE"},
		"extra field":  {`{"obligation_id":"x",` + string(act)[1:], "UNKNOWN_FIELD"},
		"currency":     {strings.Replace(string(act), `"currency":"INR"`, `"currency":"USD"`, 1), "UNSUPPORTED_CURRENCY"},
	} {
		body := []byte(`{"action":` + c.body + `,"permit_jws":"` + p.jws + `"}`)
		st, res := e.call("POST", "/v1/operations", "agent-a", body)
		want := 400
		if c.code == "UNSUPPORTED_CURRENCY" {
			want = 422
		}
		requireStatus(t, "admission "+name, st, res, want, c.code)
	}
	if e.count(`SELECT count(*) FROM steelmatter.instructions`) != 1 {
		t.Fatal("abusive input created an instruction")
	}
	requireLedger(t, e.ledger(p.mandate), before)
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatal("abusive admission changed the permit")
	}
}

// T22: actor identity cannot reach target writes or the ledger directly.
func TestT22DirectBypass(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	ctx := context.Background()

	// No HTTP path writes the target or ledger directly; owner/issuer paths reject actors.
	for _, c := range []struct{ method, path string }{{"POST", "/v1/target/purchase_orders"}, {"POST", "/v1/ledger"}, {"PUT", "/v1/mandates/" + p.mandate}} {
		st, body := e.call(c.method, c.path, "agent-a", map[string]any{})
		if st != 404 && st != 405 {
			t.Fatalf("%s %s: %d %v", c.method, c.path, st, body)
		}
	}
	st, body := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "agent-a", nil)
	requireStatus(t, "actor revokes", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/mandates", "agent-a", map[string]any{})
	if st != 400 && st != 403 {
		t.Fatalf("actor creates mandate: %d", st)
	}

	// Actors have no database role at all.
	cfg, err := pgx.ParseConfig(e.h.DB.ExecutorURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = "agent-a", "anything"
	if c, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		c.Close(ctx)
		t.Fatal("actor identity connected to the database")
	}
	// A login role without Steelmatter grants (e.g. a leaked generic login) cannot write.
	e.admin(`CREATE ROLE ` + e.h.DB.Name + `_stray LOGIN PASSWORD 'stray-synthetic'`)
	t.Cleanup(func() {
		_, _ = e.h.Admin.Exec(context.Background(), `DROP OWNED BY `+e.h.DB.Name+`_stray; DROP ROLE `+e.h.DB.Name+`_stray`)
	})
	e.admin(`GRANT CONNECT ON DATABASE ` + e.h.DB.Name + ` TO ` + e.h.DB.Name + `_stray`)
	cfg.User, cfg.Password = e.h.DB.Name+"_stray", "stray-synthetic"
	stray, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stray.Close(ctx)
	for _, sql := range []string{
		`INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id, supplier_revision, delivery_id, currency, amount_minor, created_at) VALUES ('synthetic-org','po-bypass','i','o','supplier-a',1,'warehouse-a','INR',1,now())`,
		`UPDATE steelmatter.mandates SET available_minor = limit_minor`,
		`UPDATE steelmatter.issuances SET status = 'ACTIVE'`,
	} {
		_, err := stray.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("bypass write not denied: %v (%s)", err, sql)
		}
	}
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
}

// T23: operation reads with unmapped or wrong-tenant identity disclose nothing.
func TestT23CrossScopeRead(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	st, _ := e.admit("agent-a", p.action, p.jws)
	if st != 201 {
		t.Fatal(st)
	}
	path := "/v1/operations/" + p.operation
	for _, who := range []string{"agent-b", "issuer-b", "owner-x", "agent-x", "issuer-x"} {
		st, body := e.call("GET", path, who, nil)
		requireStatus(t, "read as "+who, st, body, 404, "NOT_FOUND")
		if strings.Contains(strings.ToLower(body["message"].(string)), "po-") {
			t.Fatal("order reference disclosed")
		}
	}
	st, body := e.call("GET", path, "", nil)
	requireStatus(t, "unauthenticated", st, body, 401, "UNAUTHENTICATED")
	for _, who := range []string{"agent-a", "issuer-a", "owner-a", "observer-a", "operator-a"} {
		st, body := e.call("GET", path, who, nil)
		requireStatus(t, "read as "+who, st, body, 200, "")
	}
	// Wrong-tenant actors cannot evaluate or admit either.
	st, body = e.evaluate("agent-x", p.instruction, 1)
	requireStatus(t, "other-tenant evaluate", st, body, 404, "NOT_FOUND")
}

// T39: the action never carries obligation_id; one obligation links
// instruction, action digest and operation without rewriting the action.
func TestT39ObligationLinkage(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	ctx := context.Background()
	var before []byte
	if err := e.h.Admin.QueryRow(ctx, `SELECT action_canonical FROM steelmatter.action_revisions WHERE instruction_id = $1`, p.instruction).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "obligation") {
		t.Fatal("action carries an obligation field before commit")
	}
	st, res := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit", st, res, 201, "")

	var after, execution []byte
	var actionDigest, obligationOp, obligationInstr, obligationDigest string
	if err := e.h.Admin.QueryRow(ctx, `SELECT a.action_canonical, a.action_digest, o.execution_canonical, b.operation_id, b.instruction_id, b.action_digest
		FROM steelmatter.action_revisions a JOIN steelmatter.operations o ON o.instruction_id = a.instruction_id
		JOIN steelmatter.obligations b ON b.obligation_id = o.obligation_id WHERE a.instruction_id = $1`, p.instruction).
		Scan(&after, &actionDigest, &execution, &obligationOp, &obligationInstr, &obligationDigest); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("signed action bytes were rewritten at commit")
	}
	if obligationOp != p.operation || obligationInstr != p.instruction || obligationDigest != actionDigest || res["action_digest"] != actionDigest {
		t.Fatal("obligation linkage incomplete")
	}
	var exec map[string]any
	_ = json.Unmarshal(execution, &exec)
	if exec["obligation_id"] != res["obligation_id"] || exec["action_digest"] != actionDigest {
		t.Fatalf("execution evidence does not link obligation: %v", exec)
	}
	if e.count(`SELECT count(*) FROM steelmatter.obligations`) != 1 {
		t.Fatal("expected exactly one obligation")
	}
}

// T40: duplicate item IDs reject; proposal order normalises; signed reorder rejects.
func TestT40LineSemantics(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("dup", m, line("item-a", 1, 10), line("item-a", 2, 10)))
	requireStatus(t, "duplicate item", st, body, 400, "DUPLICATE_LINE_ITEM")

	first := e.instruction("order", m, line("item-b", 1, 10), line("item-a", 2, 10))
	st, again := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("order", m, line("item-a", 2, 10), line("item-b", 1, 10)))
	requireStatus(t, "same lines, other order", st, again, 200, "")
	if first["action_digest"] != again["action_digest"] {
		t.Fatal("proposal order changed the action digest")
	}
	lines := first["action"].(map[string]any)["lines"].([]any)
	if lines[0].(map[string]any)["item_id"] != "item-a" {
		t.Fatal("lines not normalised before authorisation")
	}
	st, ev := e.evaluate("agent-a", first["instruction_id"].(string), 1)
	requireStatus(t, "evaluate", st, ev, 201, "")
	a := clone(ev["action"].(map[string]any))
	l := a["lines"].([]any)
	a["lines"] = []any{l[1], l[0]}
	st, body = e.admit("agent-a", a, ev["permit_jws"].(string))
	requireStatus(t, "reordered signed lines", st, body, 400, "LINES_NOT_CANONICAL")
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 0 {
		t.Fatal("reordered action committed")
	}
}

// T41: unsupported versions and time formats are rejected end to end.
func TestT41VersionAndTime(t *testing.T) {
	e := newEnv(t)
	for name, exp := range map[string]string{"invalid date": "2027-02-30T10:00:00Z", "non-UTC": "2027-01-01T10:00:00+05:30", "fractional": "2027-01-01T10:00:00.5Z"} {
		st, body := e.call("POST", "/v1/mandates", "owner-a", map[string]any{"actor_ids": []string{"agent-a"}, "audience": audience,
			"supplier_ids": []string{"supplier-a"}, "delivery_ids": []string{"warehouse-a"}, "currency": "INR", "limit_minor": 1, "expires_at": exp})
		requireStatus(t, name, st, body, 400, "INVALID_TIMESTAMP")
	}
	p := e.permitted()
	a := clone(p.action)
	a["schema_version"] = "2"
	st, body := e.admit("agent-a", a, p.jws)
	requireStatus(t, "unsupported schema", st, body, 422, "UNSUPPORTED_VERSION")
	a = clone(p.action)
	a["crypto_profile"] = "SM-JWS-ES384-1"
	st, body = e.admit("agent-a", a, p.jws)
	requireStatus(t, "unsupported profile", st, body, 422, "UNSUPPORTED_PROFILE")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
}

// Revocation is idempotent and releases unused reservations (T07 serial case).
func TestRevokeIdempotentAndReleases(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	st, first := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke", st, first, 200, "")
	st, again := e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
	requireStatus(t, "revoke again", st, again, 200, "")
	if first["control_epoch"] != json.Number("2") || again["control_epoch"] != json.Number("2") {
		t.Fatalf("epoch %v / %v; want 2 once", first["control_epoch"], again["control_epoch"])
	}
	requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
	// Exactly one revocation event, recorded in the revoking transaction.
	var payload []byte
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT payload FROM steelmatter.outbox WHERE event_type = 'mandate.revoked' AND aggregate_id = $1`, p.mandate).Scan(&payload); err != nil {
		t.Fatalf("revocation event: %v", err)
	}
	var ev map[string]any
	_ = json.Unmarshal(payload, &ev)
	if ev["control_epoch"] != float64(2) || ev["released_minor"] != float64(sixLakh) || ev["owner"] != "owner-a" {
		t.Fatalf("revocation event payload %v", ev)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.outbox WHERE event_type = 'mandate.revoked'`); n != 1 {
		t.Fatalf("%d revocation events after a repeated revoke", n)
	}
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "admit after revoke", st, body, 403, "AUTHORITY_REVOKED")
	st, body = e.evaluate("agent-a", p.instruction, 1)
	requireStatus(t, "evaluate after revoke", st, body, 200, "AUTHORITY_REVOKED")
	st, body = e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-x", nil)
	requireStatus(t, "other tenant owner", st, body, 404, "NOT_FOUND")
}

// Policy denials return 200 DENY with a reason and change nothing.
func TestEvaluateDenials(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{suppliers: []string{"supplier-b"}})
	e.assign(m, "agent-a")
	inst := e.instruction("r1", m)
	st, body := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "supplier outside mandate", st, body, 200, "SUPPLIER_NOT_PERMITTED")
	if body["decision"] != "DENY" {
		t.Fatal(body)
	}
	small := e.mandate(mandateOpts{limit: 1000})
	e.assign(small, "agent-a")
	inst2 := e.instruction("r2", small)
	st, body = e.evaluate("agent-a", inst2["instruction_id"].(string), 1)
	requireStatus(t, "capacity", st, body, 200, "CAPACITY_EXHAUSTED")
	if e.count(`SELECT count(*) FROM steelmatter.issuances`) != 0 {
		t.Fatal("denials reserved capacity")
	}
}
