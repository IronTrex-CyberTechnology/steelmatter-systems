// Package httpapi defines the JSON error envelope and server-generated request IDs
// shared by the engine's HTTP endpoints.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// Error is the JSON error envelope returned by HTTP handlers.
type Error struct {
	ErrorCode   string `json:"error_code"`
	Message     string `json:"message"`
	RequestID   string `json:"request_id"`
	Retryable   bool   `json:"retryable"`
	OperationID string `json:"operation_id,omitempty"`
}

// Common handler error codes. See Catalogue for the complete set.
const (
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	CodeNotFound           = "NOT_FOUND"
)

type ctxKey struct{}

// RequestIDHeader carries the server-generated correlation ID. Client-supplied
// values are not trusted as identity and are ignored.
const RequestIDHeader = "X-Request-Id"

// WithRequestID assigns a fresh random request ID to every request.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			http.Error(w, "request id unavailable", http.StatusServiceUnavailable)
			return
		}
		id := "req_" + hex.EncodeToString(b)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// RequestID returns the request's correlation ID, or "" outside WithRequestID.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// WriteJSON writes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the error envelope.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string, retryable bool) {
	WriteJSON(w, status, Error{ErrorCode: code, Message: message, RequestID: RequestID(r.Context()), Retryable: retryable})
}
