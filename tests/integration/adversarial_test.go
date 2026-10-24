//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
)

func TestAdversarialConcurrentReplayAndTampering(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	const n = 32
	statuses := make([]int, n*2)
	bodies := make([]map[string]any, n*2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			action := clone(p.action)
			if i >= n {
				action["delivery_id"] = "forged-destination"
			}
			<-start
			statuses[i], bodies[i] = e.admit("agent-a", action, p.jws)
		}(i)
	}
	close(start)
	wg.Wait()
	created := 0
	for i, st := range statuses {
		if i >= n {
			requireStatus(t, "tampered concurrent admission", st, bodies[i], 409, "ACTION_CHANGED")
			continue
		}
		if st != 200 && st != 201 {
			t.Fatalf("identical replay %d: %d %v", i, st, bodies[i])
		}
		if st == 201 {
			created++
		}
		if bodies[i]["operation_id"] != p.operation {
			t.Fatalf("replay changed operation identity: %v", bodies[i])
		}
	}
	if created != 1 {
		t.Fatalf("%d creations, want exactly one", created)
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

func TestAdversarialRevocationAdmissionBurst(t *testing.T) {
	for round := 0; round < 5; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			e := newEnv(t)
			p := e.permitted()
			statuses := make([]int, 25)
			bodies := make([]map[string]any, 25)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range statuses {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					if i == 0 {
						statuses[i], bodies[i] = e.call("POST", "/v1/mandates/"+p.mandate+"/revoke", "owner-a", nil)
					} else {
						statuses[i], bodies[i] = e.admit("agent-a", clone(p.action), p.jws)
					}
				}(i)
			}
			close(start)
			wg.Wait()
			requireStatus(t, "revoke", statuses[0], bodies[0], 200, "")
			created, successful := 0, 0
			for i, st := range statuses[1:] {
				if st == 201 {
					created++
				}
				switch st {
				case 200, 201:
					successful++
				case 403:
					requireStatus(t, "revoked admission", st, bodies[i+1], 403, "AUTHORITY_REVOKED")
				default:
					t.Fatalf("unexpected burst result: %d %v", st, bodies[i+1])
				}
			}
			if created > 1 || (successful > 0 && created != 1) {
				t.Fatalf("inconsistent creations/replays: %d/%d", created, successful)
			}
			if created == 1 {
				requireLedger(t, e.ledger(p.mandate), committedLedger)
			} else {
				requireLedger(t, e.ledger(p.mandate), ledger{Available: limit})
			}
		})
	}
}

func TestAdversarialMalformedAdmissionsHaveNoEffects(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	valid, err := json.Marshal(map[string]any{"action": p.action, "permit_jws": p.jws})
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{
		nil, []byte(`null`), []byte(`[]`), []byte(`true`), []byte(`{}`),
		append([]byte{0xef, 0xbb, 0xbf}, valid...), append([]byte{0xff}, valid...),
		append(append([]byte{}, valid...), []byte(` {}`)...),
		[]byte(`{"action":{},"action":{},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{"x":1,"\u0078":2},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{"x":"\ud800"},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{"x":9007199254740992},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{"x":-0},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{"x":1e3},"permit_jws":"a.b.c"}`),
		[]byte(`{"action":{},"permit_jws":null}`),
		[]byte(`{"action":{},"permit_jws":"a.b.c","tenant_id":"other-org"}`),
		[]byte(`{"action":{},"permit_jws":"a.b.c","__proto__":{"approved":true}}`),
		[]byte(strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40)),
		[]byte(strings.Repeat(" ", (1<<20)+1)),
	}
	for i, raw := range cases {
		t.Run(fmt.Sprintf("input-%02d", i), func(t *testing.T) {
			st, _ := e.call("POST", "/v1/operations", "agent-a", raw)
			if st < 400 || st >= 500 {
				t.Fatalf("malformed admission returned %d, want 4xx", st)
			}
			requireLedger(t, e.ledger(p.mandate), reservedLedger)
		})
	}
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "valid control after malformed inputs", st, body, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

func TestAdversarialCrossTenantReaderRoles(t *testing.T) {
	e := newEnv(t)
	p := e.committed()
	for _, role := range []string{"OBSERVER", "OPERATOR"} {
		id := strings.ToLower(role) + "-x"
		token, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		e.admin(`INSERT INTO steelmatter.principals (principal_id, tenant_id, kind, token_sha256) VALUES ($1, 'other-org', $2, $3)`, id, role, hash)
		e.tokens[id] = token
	}
	for _, who := range []string{"owner-x", "issuer-x", "agent-x", "observer-x", "operator-x"} {
		t.Run(who, func(t *testing.T) {
			st, body, est, evidence := e.readAs(who, p.operation)
			requireStatus(t, "cross-tenant status", st, body, 404, "NOT_FOUND")
			requireStatus(t, "cross-tenant evidence", est, evidence, 404, "NOT_FOUND")
			requireSameAsAbsent(t, e, "cross-tenant status", who, body, "/v1/operations/op-absent")
			requireSameAsAbsent(t, e, "cross-tenant evidence", who, evidence, "/v1/operations/op-absent/evidence")
			st, body = e.call("POST", "/v1/operations/"+p.operation+"/reconcile", who, nil)
			requireStatus(t, "cross-tenant reconcile", st, body, 404, "NOT_FOUND")
		})
	}
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

func TestAdversarialDisabledPrincipalCannotUseIssuedPermit(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	e.admin(`UPDATE steelmatter.principals SET enabled = false WHERE principal_id = 'agent-a'`)
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "disabled actor admission", st, body, 401, "UNAUTHENTICATED")
	requireLedger(t, e.ledger(p.mandate), reservedLedger)
	e.admin(`UPDATE steelmatter.principals SET enabled = true WHERE principal_id = 'agent-a'`)
	st, body = e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "re-enabled control", st, body, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}
