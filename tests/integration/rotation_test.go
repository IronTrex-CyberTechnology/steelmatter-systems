//go:build integration

package integration

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
)

// rotatePermitKey provisions a new service key, makes it the engine's
// permit and execution signer, and retires the old one from now.
func (e *env) rotatePermitKey() string {
	e.t.Helper()
	dir := e.t.TempDir()
	k, err := keys.Provision(dir, "steelmatter-service-2", []string{"SM-PERMIT+JWS", "SM-EXECUTION+JWS"}, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	priv, err := keys.ReadPrivateKey(filepath.Join(dir, k.Name+".key.pem"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.eng.Trust.Keys = append(e.eng.Trust.Keys, k)
	for _, typ := range k.Purposes {
		e.eng.Signers[typ] = contract.Signer{Kid: k.Kid, Key: priv}
	}
	if err := e.eng.Trust.Retire("steelmatter-service", time.Now()); err != nil {
		e.t.Fatal(err)
	}
	return k.Kid
}

func (e *env) permitKid(jws string) string {
	e.t.Helper()
	v, verr := contract.Verify(jws, contract.KindPermit, e.eng.Trust)
	if verr != nil {
		e.t.Fatalf("permit does not verify: %v", verr)
	}
	return v.Kid
}

// SEC-DEF-001: after the permit key is rotated, re-evaluating an instruction
// whose live permit was signed by the retired key releases that issuance as
// KEY_RETIRED and issues a new permit with the current key (ADR-004), instead
// of returning the old permit that admission refuses.
func TestSecDef001RetryAfterKeyRotationIssuesCurrentPermit(t *testing.T) {
	e := newEnv(t)
	old := e.permitted()
	newKid := e.rotatePermitKey()

	// The old permit is refused, and its refusal tells the caller to re-evaluate.
	st, body := e.admit("agent-a", old.action, old.jws)
	requireStatus(t, "admit old permit", st, body, 403, "JWS_KEY_UNTRUSTED")

	st, body = e.evaluate("agent-a", old.instruction, 1)
	requireStatus(t, "re-evaluate after rotation", st, body, 201, "")
	permit := body["permit"].(map[string]any)
	jws := body["permit_jws"].(string)
	if permit["permit_id"] == old.permitID || permit["operation_id"] == old.operation {
		t.Fatal("re-evaluation returned the permit signed by the retired key")
	}
	if kid := e.permitKid(jws); kid != newKid {
		t.Fatalf("new permit signed by %s; want the current key %s", kid, newKid)
	}
	if status, reason := e.issuanceStatus(old.permitID); status != "RELEASED" || reason != "KEY_RETIRED" {
		t.Fatalf("old issuance %s/%s; want RELEASED/KEY_RETIRED", status, reason)
	}
	requireLedger(t, e.ledger(old.mandate), reservedLedger) // released and re-reserved, never doubled

	// The replacement is now the live permit: an identical retry returns it unchanged.
	st, body = e.evaluate("agent-a", old.instruction, 1)
	requireStatus(t, "identical retry", st, body, 200, "")
	if body["permit_jws"] != jws {
		t.Fatal("identical retry after replacement changed the permit")
	}

	st, body = e.admit("agent-a", body["action"], jws)
	requireStatus(t, "admit new permit", st, body, 201, "")
	requireLedger(t, e.ledger(old.mandate), committedLedger)
	st, body = e.admit("agent-a", old.action, old.jws)
	requireStatus(t, "old permit after commit", st, body, 403, "JWS_KEY_UNTRUSTED")

	// The superseded operation reports EXPIRED authorisation (no API change).
	st, op := e.call("GET", "/v1/operations/"+old.operation, "agent-a", nil)
	requireStatus(t, "superseded operation status", st, op, 200, "")
	if op["authorisation"] != "EXPIRED" || op["execution"] != "NOT_ADMITTED" {
		t.Fatalf("superseded operation %v", op)
	}
}
