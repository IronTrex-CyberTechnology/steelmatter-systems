//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/observer"
)

// committed returns a scenario with one committed 60,000,000 order.
func (e *env) committed() permitted {
	e.t.Helper()
	p := e.permitted()
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(e.t, "admit", st, body, 201, "")
	return p
}

func (e *env) reconcile(who, op string) (int, map[string]any) {
	e.t.Helper()
	return e.call("POST", "/v1/operations/"+op+"/reconcile", who, nil)
}

// latestObservation verifies the stored observation under the service trust
// store and returns its payload.
func (e *env) latestObservation(op string) *contract.Observation {
	e.t.Helper()
	var jws string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT observation_jws FROM steelmatter.observations WHERE operation_id = $1
		ORDER BY recorded_at DESC LIMIT 1`, op).Scan(&jws); err != nil {
		e.t.Fatal(err)
	}
	v, verr := contract.Verify(jws, contract.KindObservation, e.eng.Trust)
	if verr != nil {
		e.t.Fatalf("stored observation does not verify: %v", verr)
	}
	return v.Value.(*contract.Observation)
}

// T24: readback with changed or missing target fields never yields MATCH.
func TestT24ObserverMismatch(t *testing.T) {
	e := newEnv(t)
	p := e.committed()

	st, body := e.reconcile("observer-a", p.operation)
	requireStatus(t, "reconcile", st, body, 200, "")
	if body["observation"] != "MATCH" || body["state"] != "RESOLVED" {
		t.Fatalf("untouched destination: %v", body)
	}
	if o := e.latestObservation(p.operation); o.ObservedActionDigest == nil || *o.ObservedActionDigest != o.ExpectedActionDigest {
		t.Fatal("MATCH without equal digests")
	}

	cases := []struct {
		name, sql, want string
	}{
		{"changed supplier", `UPDATE target.purchase_orders SET supplier_id = 'supplier-b'`, "MISMATCH"},
		{"changed amount", `UPDATE target.purchase_orders SET supplier_id = 'supplier-a', amount_minor = amount_minor + 1`, "MISMATCH"},
		{"changed line", `UPDATE target.purchase_orders SET amount_minor = amount_minor - 1; UPDATE target.purchase_order_lines SET quantity = quantity + 1`, "MISMATCH"},
		{"missing lines", `DELETE FROM target.purchase_order_lines`, "INSUFFICIENT_EVIDENCE"},
		{"missing order", `ALTER TABLE target.purchase_orders DISABLE TRIGGER ALL; DELETE FROM target.purchase_orders; ALTER TABLE target.purchase_orders ENABLE TRIGGER ALL`, "INSUFFICIENT_EVIDENCE"},
	}
	for _, c := range cases {
		e.admin(c.sql)
		st, body := e.reconcile("operator-a", p.operation)
		requireStatus(t, c.name, st, body, 200, "")
		if body["observation"] != c.want {
			t.Fatalf("%s: observation %v; want %s", c.name, body["observation"], c.want)
		}
		o := e.latestObservation(p.operation)
		if o.Status != c.want {
			t.Fatalf("%s: signed status %s", c.name, o.Status)
		}
		if c.want == "INSUFFICIENT_EVIDENCE" && o.ObservedActionDigest != nil {
			t.Fatalf("%s: insufficient evidence carries an observed digest", c.name)
		}
		st, op := e.call("GET", "/v1/operations/"+p.operation, "observer-a", nil)
		requireStatus(t, c.name+" status", st, op, 200, "")
		if op["observation"] != c.want {
			t.Fatalf("%s: GET observation %v", c.name, op["observation"])
		}
	}
	// Reconcile is read-only: it never recreated the order or changed accounting.
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 0 {
		t.Fatal("reconcile created a replacement order")
	}
	l := e.ledger(p.mandate)
	if l.Committed != sixLakh || l.Reserved != 0 {
		t.Fatalf("reconcile changed accounting: %+v", l)
	}
}

// T25: observer credentials cannot execute, mutate the destination or the budget.
func TestT25ObserverSeparation(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id, supplier_revision, delivery_id, currency, amount_minor, created_at) VALUES ('synthetic-org','po-obs','i','o','supplier-a',1,'warehouse-a','INR',1,now())`,
		`UPDATE target.purchase_orders SET amount_minor = 1`,
		`DELETE FROM target.purchase_order_lines`,
		`UPDATE steelmatter.mandates SET available_minor = limit_minor`,
		`INSERT INTO steelmatter.observations (tenant_id, observation_id, operation_id, status, expected_action_digest, observation_canonical, observation_digest, observation_jws, observed_at) VALUES ('synthetic-org','x','` + p.operation + `','INSUFFICIENT_EVIDENCE',repeat('0',64),'\x7b7d',repeat('0',64),'x',now())`,
		`SELECT * FROM steelmatter.mandates`,
		`SELECT * FROM steelmatter.principals`,
	} {
		_, err := e.h.Observer.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("observer role not denied: %v\n%s", err, sql)
		}
	}
	// Observer principals cannot execute through the API either.
	st, body := e.evaluate("observer-a", p.instruction, 1)
	requireStatus(t, "observer evaluates", st, body, 403, "FORBIDDEN")
	st, body = e.admit("observer-a", p.action, p.jws)
	requireStatus(t, "observer admits", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "observer-a", nil)
	requireStatus(t, "observer revokes", st, body, 403, "FORBIDDEN")
	st, body = e.call("POST", "/v1/instructions", "observer-a", instructionBody("obs", p.mandate))
	requireStatus(t, "observer creates instruction", st, body, 403, "FORBIDDEN")
	// The observation key cannot sign anything but observations.
	if _, ok := e.eng.Signers["SM-OBSERVATION+JWS"]; ok {
		t.Fatal("the engine holds the observation key")
	}
	obsKid := e.eng.Observer.Signer.Kid
	k, _ := e.eng.Trust.Lookup(obsKid)
	if k.Allows("SM-PERMIT+JWS") || k.Allows("SM-EXECUTION+JWS") || k.Allows("SM-MANDATE+JWS") {
		t.Fatal("observation key may sign other record types")
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// T23 for evidence and reconcile: wrong-tenant or unmapped identities see nothing.
func TestT23EvidenceAndReconcileScope(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	for _, who := range []string{"agent-b", "issuer-b", "owner-x", "agent-x"} {
		st, body := e.call("GET", "/v1/operations/"+p.operation+"/evidence", who, nil)
		requireStatus(t, "evidence as "+who, st, body, 404, "NOT_FOUND")
		if _, leaked := body["records"]; leaked {
			t.Fatal("records disclosed")
		}
	}
	for _, who := range []string{"agent-a", "issuer-a", "owner-a", "owner-x", "agent-x"} {
		st, body := e.reconcile(who, p.operation)
		requireStatus(t, "reconcile as "+who, st, body, 404, "NOT_FOUND")
	}
	for _, who := range []string{"agent-a", "issuer-a", "owner-a", "observer-a", "operator-a"} {
		st, body := e.call("GET", "/v1/operations/"+p.operation+"/evidence", who, nil)
		requireStatus(t, "evidence as "+who, st, body, 200, "")
		raw, _ := json.Marshal(body)
		for _, secret := range []string{"PRIVATE KEY", `"d":`, "smt_", "password"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("evidence contains secret material %q", secret)
			}
		}
	}
}

// verifyWithCLI runs the standalone TypeScript verifier on a bundle with the
// service trust store as the explicit anchor.
func (e *env) verifyWithCLI(bundle map[string]any) map[string]any {
	e.t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		e.t.Fatal("node is required")
	}
	dir := e.t.TempDir()
	b, _ := json.Marshal(bundle)
	path := filepath.Join(dir, "bundle.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(node, "../../packages/verifier/src/cli.ts", "verify", "--trust", filepath.Join(e.keysDir, "trust-store.json"), "--bundle", path, "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			e.t.Fatalf("verifier CLI failed: %v\n%s", err, ee.Stderr)
		}
		e.t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		e.t.Fatalf("verifier output: %v\n%s", err, out)
	}
	return report
}

func record(report map[string]any, kind string) map[string]any {
	return report["records"].(map[string]any)[kind].(map[string]any)
}

// End to end: evidence exported by the Go service verifies in the TypeScript
// verifier (T21 runtime), and tampering or missing records are reported (T26).
func TestEvidenceExportVerifiesInStandaloneVerifier(t *testing.T) {
	e := newEnv(t)
	p := e.committed()

	// Before observation: execution signed, observation missing → incomplete.
	st, b := e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	requireStatus(t, "evidence", st, b, 200, "")
	if b["evidence"] != "AVAILABLE" || len(b["incomplete"].([]any)) != 1 {
		t.Fatalf("evidence before observation: %v %v", b["evidence"], b["incomplete"])
	}
	r := e.verifyWithCLI(b)
	if r["linkage"].(map[string]any)["status"] != "INCOMPLETE" || record(r, "observation")["integrity"] != "MISSING" {
		t.Fatalf("missing observation not reported as incomplete: %v", r["linkage"])
	}

	// After observation: every record verifies and links.
	if st, body := e.reconcile("observer-a", p.operation); st != 200 || body["observation"] != "MATCH" {
		t.Fatalf("reconcile %d %v", st, body)
	}
	_, b = e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	r = e.verifyWithCLI(b)
	for _, kind := range []string{"mandate", "permit", "execution", "observation"} {
		rec := record(r, kind)
		if rec["integrity"] != "VALID" || rec["issuer"] != "TRUSTED" || rec["profile"] != "SUPPORTED" || rec["payload"] != "VALID" {
			t.Fatalf("%s: %v", kind, rec)
		}
	}
	if r["linkage"].(map[string]any)["status"] != "COMPLETE" || r["observation"].(map[string]any)["status"] != "MATCH" ||
		r["observation"].(map[string]any)["observed_matches_action"] != true {
		t.Fatalf("report: linkage %v observation %v", r["linkage"], r["observation"])
	}

	// Tampering with stored evidence: the service reports INVALID and the
	// verifier rejects the record.
	var execJWS string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT execution_jws FROM steelmatter.operations WHERE operation_id = $1`, p.operation).Scan(&execJWS)
	parts := strings.Split(execJWS, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	forged := strings.Replace(string(payload), `"amount_minor":60000000`, `"amount_minor":6000000`, 1)
	e.adminBypass(`UPDATE steelmatter.operations SET execution_jws = $1 WHERE operation_id = $2`,
		parts[0]+"."+base64.RawURLEncoding.EncodeToString([]byte(forged))+"."+parts[2], p.operation)
	st, op := e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "status after tamper", st, op, 200, "")
	if op["evidence"] != "INVALID" {
		t.Fatalf("service evidence status %v after tampering", op["evidence"])
	}
	_, b = e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	r = e.verifyWithCLI(b)
	if rec := record(r, "execution"); rec["integrity"] != "INVALID" || rec["code"] != "JWS_SIGNATURE_INVALID" {
		t.Fatalf("tampered execution: %v", rec)
	}
	if r["linkage"].(map[string]any)["status"] == "COMPLETE" {
		t.Fatal("linkage complete despite a tampered record")
	}
}

// Execution evidence can be pending after commit and is signed later over
// the unchanged stored bytes.
func TestEvidencePendingThenSigned(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	var before []byte
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT execution_canonical FROM steelmatter.operations WHERE operation_id = $1`, p.operation).Scan(&before)
	e.adminBypass(`UPDATE steelmatter.operations SET execution_jws = NULL WHERE operation_id = $1`, p.operation)
	st, b := e.call("GET", "/v1/operations/"+p.operation+"/evidence", "agent-a", nil)
	requireStatus(t, "pending evidence", st, b, 200, "")
	if b["evidence"] != "PENDING" || !strings.Contains(strings.Join(toStrings(b["incomplete"]), ","), "execution_jws") {
		t.Fatalf("pending evidence: %v %v", b["evidence"], b["incomplete"])
	}
	if n, err := e.eng.SignPendingExecutions(context.Background()); err != nil || n != 1 {
		t.Fatalf("signed %d (%v)", n, err)
	}
	var jws string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT execution_jws FROM steelmatter.operations WHERE operation_id = $1`, p.operation).Scan(&jws)
	v, verr := contract.Verify(jws, contract.KindExecution, e.eng.Trust)
	if verr != nil || string(v.Payload) != string(before) {
		t.Fatalf("late execution signature not over the stored bytes: %v", verr)
	}
	if n, err := e.eng.ObserveUnobserved(context.Background()); err != nil || n != 1 {
		t.Fatalf("observed %d (%v)", n, err)
	}
	st, op := e.call("GET", "/v1/operations/"+p.operation, "agent-a", nil)
	requireStatus(t, "status", st, op, 200, "")
	if op["evidence"] != "AVAILABLE" || op["observation"] != "MATCH" {
		t.Fatalf("status %v", op)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// F-09: the startup check refuses an observer login that is the executor or
// that can write.
func TestObserverSeparationCheck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := observer.CheckSeparation(ctx, e.h.Observer, e.h.Executor); err != nil {
		t.Fatalf("correct roles rejected: %v", err)
	}
	if err := observer.CheckSeparation(ctx, e.h.Executor, e.h.Executor); err == nil {
		t.Fatal("shared role accepted")
	}
	if err := observer.CheckSeparation(ctx, e.h.Executor, e.h.Observer); err == nil {
		t.Fatal("a writable observer role was accepted")
	}
}

// F-10: a validly signed record from another operation does not make this
// operation's evidence AVAILABLE; it is reported INVALID.
func TestEvidenceStatusChecksLinkageNotJustSignatures(t *testing.T) {
	e := newEnv(t)
	a := e.committed()
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("second-record", m)
	st, ev := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate b", st, ev, 201, "")
	st, b := e.admit("agent-a", ev["action"], ev["permit_jws"].(string))
	requireStatus(t, "admit b", st, b, 201, "")

	var foreign string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT execution_jws FROM steelmatter.operations WHERE operation_id = $1`, b["operation_id"]).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if _, verr := contract.Verify(foreign, contract.KindExecution, e.eng.Trust); verr != nil {
		t.Fatal("the foreign record must itself be validly signed for this test")
	}
	e.adminBypass(`UPDATE steelmatter.operations SET execution_jws = $1 WHERE operation_id = $2`, foreign, a.operation)
	st, op := e.call("GET", "/v1/operations/"+a.operation, "agent-a", nil)
	requireStatus(t, "status", st, op, 200, "")
	if op["evidence"] != "INVALID" {
		t.Fatalf("evidence %v with another operation's signed execution record", op["evidence"])
	}
	st, bundle := e.call("GET", "/v1/operations/"+a.operation+"/evidence", "agent-a", nil)
	requireStatus(t, "evidence", st, bundle, 200, "")
	if bundle["evidence"] != "INVALID" {
		t.Fatalf("bundle evidence %v", bundle["evidence"])
	}
	r := e.verifyWithCLI(bundle)
	if r["linkage"].(map[string]any)["status"] != "BROKEN" {
		t.Fatalf("standalone verifier linkage %v", r["linkage"])
	}
}

// I02 checklist item 5, historical trust keys: after the service key is
// retired, evidence it signed earlier still verifies (marked RETIRED, signed
// within validity), but it can no longer authorise an admission or sign a
// new permit.
func TestHistoricalTrustKeys(t *testing.T) {
	e := newEnv(t)
	a := e.committed()
	if st, _ := e.reconcile("observer-a", a.operation); st != 200 {
		t.Fatal(st)
	}
	// A permit issued before retirement but not yet used.
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	inst := e.instruction("issued-before-retirement", m)
	st, b := e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate b", st, b, 201, "")

	// Retire the service key (permits and executions) from the next second,
	// in the running engine and in the trust store file a verifier uses.
	at := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	if err := e.eng.Trust.Retire("steelmatter-service", at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.keysDir, "trust-store.json")
	ts, err := keys.LoadTrustStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Retire("steelmatter-service", at); err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(at) + 50*time.Millisecond)

	// Historical evidence stays valid in the service and the standalone verifier.
	st, op := e.call("GET", "/v1/operations/"+a.operation, "agent-a", nil)
	requireStatus(t, "status after retirement", st, op, 200, "")
	if op["evidence"] != "AVAILABLE" {
		t.Fatalf("historical evidence %v after key retirement", op["evidence"])
	}
	_, bundle := e.call("GET", "/v1/operations/"+a.operation+"/evidence", "agent-a", nil)
	r := e.verifyWithCLI(bundle)
	for _, kind := range []string{"permit", "execution"} {
		rec := record(r, kind)
		if rec["integrity"] != "VALID" || rec["issuer"] != "TRUSTED" || rec["key_status"] != "RETIRED" || rec["signed_within_key_validity"] != true {
			t.Fatalf("%s after retirement: %v", kind, rec)
		}
	}
	if r["linkage"].(map[string]any)["status"] != "COMPLETE" {
		t.Fatalf("linkage %v", r["linkage"])
	}

	// The retired key can no longer authorise or sign anything new.
	st, body := e.admit("agent-a", b["action"], b["permit_jws"].(string))
	requireStatus(t, "admit with permit from retired key", st, body, 403, "JWS_KEY_UNTRUSTED")
	inst3 := e.instruction("after-retirement", m)
	before := e.count(`SELECT count(*) FROM steelmatter.issuances`)
	st, body = e.evaluate("agent-a", inst3["instruction_id"].(string), 1)
	requireStatus(t, "evaluate with only a retired permit key", st, body, 503, "SERVICE_UNAVAILABLE")
	if e.count(`SELECT count(*) FROM steelmatter.issuances`) != before {
		t.Fatal("evaluation reserved capacity although no current signing key exists")
	}
	if e.count(`SELECT count(*) FROM target.purchase_orders`) != 1 {
		t.Fatal("an order was committed with a retired key")
	}
}
