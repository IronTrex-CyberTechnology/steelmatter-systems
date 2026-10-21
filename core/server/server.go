// Package server wires the HTTP surface: liveness, database readiness and
// the product routes registered by core/api.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/httpapi"
)

// Pinger reports whether the service can admit work (database reachable and,
// with the engine, clocks healthy).
type Pinger interface {
	Ping(ctx context.Context) error
}

// ReadyFunc adapts a function to Pinger, e.g. server.ReadyFunc(engine.Ready).
type ReadyFunc func(ctx context.Context) error

// Ping calls f.
func (f ReadyFunc) Ping(ctx context.Context) error { return f(ctx) }

// RequestTimeout bounds every request; the HTTP server's WriteTimeout must
// exceed it so the error envelope can still be written.
const RequestTimeout = 60 * time.Second

// Info is reported by the health endpoint.
type Info struct {
	Version  string `json:"version"`
	Baseline string `json:"baseline"`
}

// New returns the service handler. register adds further routes (the
// product API) to the same mux.
func New(info Info, db Pinger, register ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	for _, r := range register {
		r(mux)
	}
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": info.Version, "baseline": info.Baseline})
	})
	mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			// Fail closed: no admission without the authoritative database.
			httpapi.WriteError(w, r, http.StatusServiceUnavailable, httpapi.CodeServiceUnavailable, "authoritative database unavailable", true)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteError(w, r, http.StatusNotFound, httpapi.CodeNotFound, "not found", false)
	})
	return httpapi.WithRequestID(recoverPanics(withDeadline(mux)))
}

// recoverPanics turns an unexpected panic into the INTERNAL_ERROR envelope
// (F-21). http.ErrAbortHandler is re-raised: it deliberately drops the
// connection without a response.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic in handler", "path", r.URL.Path, "request_id", httpapi.RequestID(r.Context()), "panic", v, "stack", string(debug.Stack()))
				httpapi.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected internal error", false)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func withDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
