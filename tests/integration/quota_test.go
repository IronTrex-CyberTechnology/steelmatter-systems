//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/api"
)

// Document 03 Errors: 429 quota. Over-quota requests get the error envelope
// with RATE_LIMITED, retryable=true and Retry-After, and have no effect;
// other principals keep their own quota; unauthenticated requests stay 401.
func TestQuotaReturns429WithoutEffect(t *testing.T) {
	e := newEnv(t, envOpts{limiter: api.NewLimiter(1, 2)}) // burst 2, then one per minute
	m := e.mandate(mandateOpts{})                          // owner-a: 1 of 2
	e.assign(m, "agent-a")

	st, body := e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-quota-1", m))
	requireStatus(t, "first instruction", st, body, 201, "")
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-quota-2", m))
	requireStatus(t, "second instruction", st, body, 201, "")

	before := e.count(`SELECT count(*) FROM steelmatter.instructions`)
	req, _ := http.NewRequest("POST", e.url+"/v1/instructions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+e.tokens["issuer-a"])
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("over quota: status %d, Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	st, body = e.call("POST", "/v1/instructions", "issuer-a", instructionBody("po-quota-3", m))
	requireStatus(t, "over quota", st, body, 429, "RATE_LIMITED")
	if body["retryable"] != true || body["request_id"] == "" {
		t.Fatalf("envelope %v", body)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.instructions`); n != before {
		t.Fatalf("rate-limited request created %d instructions", n-before)
	}

	// owner-a has one request left; agent-a has its own bucket.
	st, body = e.call("GET", "/v1/operations/op-none", "agent-a", nil)
	requireStatus(t, "other principal", st, body, 404, "NOT_FOUND")
	st, body = e.call("POST", "/v1/mandates/"+m+"/revoke", "owner-a", nil)
	requireStatus(t, "owner second request", st, body, 200, "")
	st, body = e.call("POST", "/v1/mandates/"+m+"/revoke", "owner-a", nil)
	requireStatus(t, "owner over quota", st, body, 429, "RATE_LIMITED")

	st, body = e.call("POST", "/v1/instructions", "", instructionBody("po-quota-4", m))
	requireStatus(t, "unauthenticated", st, body, 401, "UNAUTHENTICATED")
}
