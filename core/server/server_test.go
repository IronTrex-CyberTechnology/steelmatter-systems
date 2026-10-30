package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/httpapi"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthReportsVersionAndRequestID(t *testing.T) {
	rec := do(t, New(Info{Version: "dev", Baseline: "0.6"}, fakePinger{}), http.MethodGet, "/v1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if id := rec.Header().Get(httpapi.RequestIDHeader); !strings.HasPrefix(id, "req_") {
		t.Fatalf("request id %q", id)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["baseline"] != "0.6" {
		t.Fatalf("body %s", rec.Body)
	}
}

func TestReadyFailsClosedWhenDatabaseUnavailable(t *testing.T) {
	rec := do(t, New(Info{}, fakePinger{err: errors.New("down")}), http.MethodGet, "/v1/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	var e httpapi.Error
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.ErrorCode != httpapi.CodeServiceUnavailable || !e.Retryable || e.RequestID != rec.Header().Get(httpapi.RequestIDHeader) {
		t.Fatalf("error envelope %+v", e)
	}
	if strings.Contains(rec.Body.String(), "down") {
		t.Fatal("internal error detail disclosed")
	}
}

func TestUnknownRouteUsesErrorEnvelope(t *testing.T) {
	rec := do(t, New(Info{}, fakePinger{}), http.MethodGet, "/v1/nope")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error_code":"NOT_FOUND"`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestClientRequestIDIsNotTrusted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set(httpapi.RequestIDHeader, "attacker-chosen")
	rec := httptest.NewRecorder()
	New(Info{}, fakePinger{}).ServeHTTP(rec, req)
	if rec.Header().Get(httpapi.RequestIDHeader) == "attacker-chosen" {
		t.Fatal("client request id echoed as server id")
	}
}

func TestPanicReturnsErrorEnvelope(t *testing.T) {
	h := New(Info{}, fakePinger{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	})
	rec := do(t, h, http.MethodGet, "/boom")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error_code":"INTERNAL_ERROR"`) || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestAbortHandlerStillDropsConnection(t *testing.T) {
	h := New(Info{}, fakePinger{}, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	})
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler was swallowed (recovered %v)", v)
		}
	}()
	do(t, h, http.MethodGet, "/abort")
}

type reasonErr string

func (r reasonErr) Error() string         { return "internal: " + string(r) }
func (r reasonErr) PublicMessage() string { return string(r) }

// NEW-QA-04: readiness stays 503 but names the reason the check gives.
func TestReadyNamesTheReason(t *testing.T) {
	rec := do(t, New(Info{}, fakePinger{err: fmt.Errorf("ready: %w", reasonErr("recovery anchor unavailable"))}), http.MethodGet, "/v1/ready")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"recovery anchor unavailable"`) ||
		!strings.Contains(rec.Body.String(), `"error_code":"SERVICE_UNAVAILABLE"`) || strings.Contains(rec.Body.String(), "internal:") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}
