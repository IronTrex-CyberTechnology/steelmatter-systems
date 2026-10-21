// Package api maps the document 03 endpoints (api/openapi.yaml) onto the
// engine. Order of checks per request: authentication (401), per-principal
// quota (429, when configured), strict closed
// body parsing (400/422), then engine authorisation and state checks.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/auth"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/engine"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/httpapi"
)

// Options configures the product endpoints.
type Options struct {
	// Limiter enforces the per-principal quota (429 RATE_LIMITED); nil = none.
	Limiter *Limiter
}

// Register adds the product endpoints to mux without a quota.
func Register(mux *http.ServeMux, e *engine.Engine, a *auth.Authenticator) {
	RegisterWith(mux, e, a, Options{})
}

// RegisterWith adds the product endpoints to mux with options.
func RegisterWith(mux *http.ServeMux, e *engine.Engine, a *auth.Authenticator, o Options) {
	h := &handlers{e: e, a: a, lim: o.Limiter}
	mux.HandleFunc("POST /v1/mandates", h.wrap(h.createMandate))
	mux.HandleFunc("POST /v1/mandates/{mandate_id}/revoke", h.wrap(h.revokeMandate))
	mux.HandleFunc("POST /v1/instructions", h.wrap(h.createInstruction))
	mux.HandleFunc("POST /v1/instructions/{instruction_id}/revisions", h.wrap(h.amendInstruction))
	mux.HandleFunc("POST /v1/actions/evaluate", h.wrap(h.evaluate))
	mux.HandleFunc("POST /v1/actions/simulate", h.wrap(h.simulate))
	mux.HandleFunc("POST /v1/operations", h.wrap(h.admit))
	mux.HandleFunc("GET /v1/operations/{operation_id}", h.wrap(h.getOperation))
	mux.HandleFunc("POST /v1/operations/{operation_id}/reconcile", h.wrap(h.reconcile))
	mux.HandleFunc("GET /v1/operations/{operation_id}/evidence", h.wrap(h.evidence))
	mux.HandleFunc("GET /v1/adapters/{adapter_id}/assurance", h.wrap(h.assurance))
}

type handlers struct {
	e   *engine.Engine
	a   *auth.Authenticator
	lim *Limiter
}

type response struct {
	status int
	body   any
}

type handlerFunc func(ctx context.Context, p auth.Principal, r *http.Request) (*response, error)

func (h *handlers) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := h.a.FromHeader(r.Context(), r.Header.Get("Authorization"))
		if errors.Is(err, auth.ErrUnauthenticated) {
			httpapi.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid credentials", false)
			return
		}
		if err != nil {
			writeFault(w, r, &engine.Fault{Code: "SERVICE_UNAVAILABLE", Message: "authoritative database unavailable"})
			return
		}
		if h.lim != nil {
			if ok, wait := h.lim.Allow(p.ID); !ok {
				w.Header().Set("Retry-After", strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10))
				writeFault(w, r, &engine.Fault{Code: "RATE_LIMITED", Message: "request quota exceeded; retry later"})
				return
			}
		}
		if r.Method == http.MethodPost && r.ContentLength != 0 {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeFault(w, r, &engine.Fault{Code: "INVALID_JSON", Message: "Content-Type must be application/json"})
				return
			}
		}
		res, err := fn(r.Context(), p, r)
		if err != nil {
			if f, ok := engine.AsFault(err); ok {
				writeFault(w, r, f)
				return
			}
			slog.Error("internal error", "path", r.URL.Path, "request_id", httpapi.RequestID(r.Context()), "err", err)
			writeFault(w, r, &engine.Fault{Code: "INTERNAL_ERROR", Message: "unexpected internal error"})
			return
		}
		httpapi.WriteJSON(w, res.status, res.body)
	}
}

// maxMessage caps error messages; detail that would exceed it (for example
// echoed field names from a hostile body) is truncated, never reflected in full (F-20).
const maxMessage = 200

func writeFault(w http.ResponseWriter, r *http.Request, f *engine.Fault) {
	spec, ok := httpapi.Lookup(f.Code)
	if !ok || spec.Use == httpapi.UseDeny {
		slog.Error("unmapped fault code", "code", f.Code)
		spec, _ = httpapi.Lookup("INTERNAL_ERROR")
		f = &engine.Fault{Code: spec.Code, Message: "unexpected internal error", OperationID: f.OperationID}
	}
	msg := strings.ToValidUTF8(f.Message, "")
	if len(msg) > maxMessage {
		msg = strings.ToValidUTF8(msg[:maxMessage], "") + "…"
	}
	httpapi.WriteJSON(w, spec.Status, httpapi.Error{ErrorCode: f.Code, Message: msg,
		RequestID: httpapi.RequestID(r.Context()), Retryable: spec.Retryable, OperationID: f.OperationID})
}

// ---- strict request parsing ----

type obj = map[string]any

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

func bad(code, format string, a ...any) error {
	return &engine.Fault{Code: code, Message: fmt.Sprintf(format, a...)}
}

func readBody(r *http.Request, allowEmpty bool) (obj, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, contract.MaxPayloadBytes+1))
	if err != nil {
		return nil, bad("INVALID_JSON", "could not read body")
	}
	if len(b) == 0 && allowEmpty {
		return obj{}, nil
	}
	v, perr := contract.ParseStrict(b)
	if perr != nil {
		return nil, bad(string(perr.Code), "%s", perr.Detail)
	}
	o, ok := v.(obj)
	if !ok {
		return nil, bad("SCHEMA_VIOLATION", "body must be a JSON object")
	}
	return o, nil
}

// closed checks the field set: unknown fields first, then required ones.
func closed(o obj, path string, required, optional []string) error {
	allowed := map[string]bool{}
	for _, f := range append(append([]string{}, required...), optional...) {
		allowed[f] = true
	}
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return bad("UNKNOWN_FIELD", "field %s%s is not accepted", path, k)
		}
	}
	for _, f := range required {
		if _, ok := o[f]; !ok {
			return bad("SCHEMA_VIOLATION", "field %s%s is required", path, f)
		}
	}
	return nil
}

func ident(o obj, key, path string) (string, error) {
	s, ok := o[key].(string)
	if !ok || len(s) == 0 || len(s) > 128 || !identifier.MatchString(s) {
		return "", bad("SCHEMA_VIOLATION", "%s%s must be an identifier", path, key)
	}
	return s, nil
}

func integer(o obj, key, path string, min int64) (int64, error) {
	n, ok := o[key].(int64)
	if !ok || n < min || n > contract.MaxSafeInteger {
		return 0, bad("SCHEMA_VIOLATION", "%s%s must be an integer in [%d, 2^53-1]", path, key, min)
	}
	return n, nil
}

func identList(o obj, key string) ([]string, error) {
	arr, ok := o[key].([]any)
	if !ok || len(arr) < 1 || len(arr) > 100 {
		return nil, bad("SCHEMA_VIOLATION", "%s must be an array of 1..100 identifiers", key)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok || !identifier.MatchString(s) || len(s) > 128 {
			return nil, bad("SCHEMA_VIOLATION", "%s must contain identifiers", key)
		}
		if seen[s] {
			return nil, bad("SCHEMA_VIOLATION", "%s must not repeat %s", key, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

func currency(o obj, path string) error {
	c, ok := o["currency"].(string)
	if !ok {
		return bad("SCHEMA_VIOLATION", "%scurrency must be a string", path)
	}
	if c != contract.Currency {
		return bad("UNSUPPORTED_CURRENCY", "only INR is supported")
	}
	return nil
}

func proposed(o obj, key string) (engine.ProposedAction, error) {
	var pa engine.ProposedAction
	po, ok := o[key].(obj)
	if !ok {
		return pa, bad("SCHEMA_VIOLATION", "%s must be an object", key)
	}
	path := key + "."
	if err := closed(po, path, []string{"supplier_id", "delivery_id", "currency", "lines"}, nil); err != nil {
		return pa, err
	}
	var err error
	if pa.SupplierID, err = ident(po, "supplier_id", path); err != nil {
		return pa, err
	}
	if pa.DeliveryID, err = ident(po, "delivery_id", path); err != nil {
		return pa, err
	}
	if err := currency(po, path); err != nil {
		return pa, err
	}
	pa.Currency = contract.Currency
	lines, ok := po["lines"].([]any)
	if !ok || len(lines) < 1 || len(lines) > 100 {
		return pa, bad("SCHEMA_VIOLATION", "%slines must be an array of 1..100 lines", path)
	}
	for i, l := range lines {
		lo, ok := l.(obj)
		lp := fmt.Sprintf("%slines[%d].", path, i)
		if !ok {
			return pa, bad("SCHEMA_VIOLATION", "%s must be an object", lp)
		}
		if err := closed(lo, lp, []string{"item_id", "quantity", "unit_price_minor"}, nil); err != nil {
			return pa, err
		}
		var line contract.Line
		if line.ItemID, err = ident(lo, "item_id", lp); err != nil {
			return pa, err
		}
		if line.Quantity, err = integer(lo, "quantity", lp, 1); err != nil {
			return pa, err
		}
		if line.UnitPriceMinor, err = integer(lo, "unit_price_minor", lp, 0); err != nil {
			return pa, err
		}
		pa.Lines = append(pa.Lines, line)
	}
	return pa, nil
}

// ---- handlers ----

func (h *handlers) createMandate(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"actor_ids", "audience", "supplier_ids", "delivery_ids", "currency", "limit_minor", "expires_at"}, nil); err != nil {
		return nil, err
	}
	var req engine.MandateRequest
	if req.ActorIDs, err = identList(o, "actor_ids"); err != nil {
		return nil, err
	}
	if req.SupplierIDs, err = identList(o, "supplier_ids"); err != nil {
		return nil, err
	}
	if req.DeliveryIDs, err = identList(o, "delivery_ids"); err != nil {
		return nil, err
	}
	if req.Audience, err = ident(o, "audience", ""); err != nil {
		return nil, err
	}
	if err := currency(o, ""); err != nil {
		return nil, err
	}
	if req.LimitMinor, err = integer(o, "limit_minor", "", 1); err != nil {
		return nil, err
	}
	exp, ok := o["expires_at"].(string)
	if !ok {
		return nil, bad("SCHEMA_VIOLATION", "expires_at must be a string")
	}
	req.ExpiresAt = exp
	v, err := h.e.CreateMandate(ctx, p, req)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusCreated, v}, nil
}

func (h *handlers) revokeMandate(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, true)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", nil, nil); err != nil {
		return nil, err
	}
	id := r.PathValue("mandate_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "mandate not found")
	}
	v, err := h.e.RevokeMandate(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) createInstruction(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"source_system", "source_record_id", "mandate_id", "proposed_action"}, []string{"client_request_id"}); err != nil {
		return nil, err
	}
	var req engine.InstructionRequest
	if req.SourceSystem, err = ident(o, "source_system", ""); err != nil {
		return nil, err
	}
	if req.SourceRecordID, err = ident(o, "source_record_id", ""); err != nil {
		return nil, err
	}
	if req.MandateID, err = ident(o, "mandate_id", ""); err != nil {
		return nil, err
	}
	if _, present := o["client_request_id"]; present {
		if req.ClientRequestID, err = ident(o, "client_request_id", ""); err != nil {
			return nil, err
		}
	}
	if req.Proposed, err = proposed(o, "proposed_action"); err != nil {
		return nil, err
	}
	v, created, err := h.e.CreateInstruction(ctx, p, req)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return &response{status, v}, nil
}

func (h *handlers) amendInstruction(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"amendment_id", "expected_action_revision", "proposed_action"}, nil); err != nil {
		return nil, err
	}
	var req engine.AmendmentRequest
	if req.AmendmentID, err = ident(o, "amendment_id", ""); err != nil {
		return nil, err
	}
	if req.ExpectedRevision, err = integer(o, "expected_action_revision", "", 1); err != nil {
		return nil, err
	}
	if req.Proposed, err = proposed(o, "proposed_action"); err != nil {
		return nil, err
	}
	id := r.PathValue("instruction_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "instruction not found")
	}
	v, created, err := h.e.AmendInstruction(ctx, p, id, req)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return &response{status, v}, nil
}

func (h *handlers) evaluate(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"instruction_id", "action_revision"}, nil); err != nil {
		return nil, err
	}
	id, err := ident(o, "instruction_id", "")
	if err != nil {
		return nil, err
	}
	rev, err := integer(o, "action_revision", "", 1)
	if err != nil {
		return nil, err
	}
	v, err := h.e.Evaluate(ctx, p, id, rev)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if v.Created {
		status = http.StatusCreated
	}
	return &response{status, v}, nil
}

func (h *handlers) admit(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"action", "permit_jws"}, nil); err != nil {
		return nil, err
	}
	action, ok := o["action"].(obj)
	if !ok {
		return nil, bad("SCHEMA_VIOLATION", "action must be an object")
	}
	jws, ok := o["permit_jws"].(string)
	if !ok {
		return nil, bad("SCHEMA_VIOLATION", "permit_jws must be a string")
	}
	// The body already passed the strict parser; re-serialising the action
	// subtree is lossless (strings, exact integers, arrays, objects).
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return nil, err
	}
	v, err := h.e.Admit(ctx, p, actionJSON, jws)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if v.Created {
		status = http.StatusCreated
	}
	return &response{status, v}, nil
}

func (h *handlers) getOperation(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	id := r.PathValue("operation_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "operation not found")
	}
	v, err := h.e.GetOperation(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) reconcile(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, true)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", nil, nil); err != nil {
		return nil, err
	}
	id := r.PathValue("operation_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "operation not found")
	}
	v, err := h.e.Reconcile(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) evidence(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	id := r.PathValue("operation_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "operation not found")
	}
	v, err := h.e.Evidence(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) simulate(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"instruction_id", "action_revision"}, nil); err != nil {
		return nil, err
	}
	id, err := ident(o, "instruction_id", "")
	if err != nil {
		return nil, err
	}
	rev, err := integer(o, "action_revision", "", 1)
	if err != nil {
		return nil, err
	}
	v, err := h.e.Simulate(ctx, p, id, rev)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) assurance(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	id := r.PathValue("adapter_id")
	if !identifier.MatchString(id) {
		return nil, bad("NOT_FOUND", "adapter not found")
	}
	v, err := h.e.Assurance(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}
