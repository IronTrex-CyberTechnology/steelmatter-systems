package api

// R1 endpoints (api/openapi.yaml, tags dispatches and recovery): dispatch to
// a real destination, settle, HELD findings, the two-step repair, anchor
// repair and the restore/resume gate. They map one to one onto the engine
// calls the integration tests exercise; the HTTP layer adds strict parsing,
// principal-kind checks and the configured destination connector, nothing
// else. Who may review or confirm a repair is decided by the engine
// (STEELMATTER_REPAIR_REVIEWERS / _CONFIRMERS), not here.
//
// The recovery endpoints act on the whole engine instance (the write switch
// is instance-wide). R1 runs one tenant per instance.

import (
	"context"
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

func (h *handlers) registerR1(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/dispatches", h.wrap(h.dispatch))
	mux.HandleFunc("GET /v1/dispatches/{op_ref}", h.wrap(h.getDispatch))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/settle", h.wrap(h.settle))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/findings", h.wrap(h.recordFinding))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/repairs", h.wrap(h.prepareRepair))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/repairs/{repair_id}/review", h.wrap(h.reviewRepair))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/repairs/{repair_id}/confirm", h.wrap(h.confirmRepair))
	mux.HandleFunc("POST /v1/dispatches/{op_ref}/anchor-repair", h.wrap(h.anchorRepair))
	mux.HandleFunc("GET /v1/recovery/write-state", h.wrap(h.writeState))
	mux.HandleFunc("POST /v1/recovery/check", h.wrap(h.checkRestore))
	mux.HandleFunc("POST /v1/recovery/reconcile", h.wrap(h.reconcileRestore))
	mux.HandleFunc("POST /v1/recovery/resume", h.wrap(h.resume))
}

func requireKind(p auth.Principal, kinds ...string) error {
	for _, k := range kinds {
		if p.Kind == k {
			return nil
		}
	}
	return bad("FORBIDDEN", "principal kind %s may not call this endpoint", p.Kind)
}

// conn returns the configured destination connector. Nil is a typed nil
// interface guard: an engine without a connector cannot settle or recover.
func (h *handlers) conn() (connector.Connector, error) {
	if h.e.Connector == nil {
		return nil, bad("SERVICE_UNAVAILABLE", "no destination connector is configured")
	}
	return h.e.Connector, nil
}

func opRef(r *http.Request) (string, error) {
	id := r.PathValue("op_ref")
	if len(id) > 128 || !identifier.MatchString(id) {
		return "", bad("NOT_FOUND", "operation not found")
	}
	return id, nil
}

// text reads a free-text field of 1..max characters (findings, notes,
// evidence references, which may be URLs).
func text(o obj, key string, max int) (string, error) {
	s, ok := o[key].(string)
	if !ok || s == "" || utf8.RuneCountInString(s) > max {
		return "", bad("SCHEMA_VIOLATION", "%s must be a string of 1..%d characters", key, max)
	}
	return s, nil
}

func emptyBody(r *http.Request) error {
	o, err := readBody(r, true)
	if err != nil {
		return err
	}
	return closed(o, "", nil, nil)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (h *handlers) dispatch(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
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
	actionJSON, err := json.Marshal(action) // lossless after the strict parser, as in admit
	if err != nil {
		return nil, err
	}
	v, err := h.e.Dispatch(ctx, p, actionJSON, jws)
	if err != nil {
		return nil, err
	}
	status := http.StatusOK
	if v.Created {
		status = http.StatusCreated
	}
	return &response{status, v}, nil
}

func (h *handlers) getDispatch(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Observer, auth.Operator); err != nil {
		return nil, err
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	v, err := h.e.GetDispatch(ctx, p.Tenant, ref)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, bad("NOT_FOUND", "operation not found")
	}
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) settle(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil {
		return nil, err
	}
	if err := emptyBody(r); err != nil {
		return nil, err
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	c, err := h.conn()
	if err != nil {
		return nil, err
	}
	v, err := h.e.Settle(ctx, p.Tenant, ref, c)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

type findingResponse struct {
	OpRef     string `json:"op_ref"`
	AnchorSeq int64  `json:"anchor_seq"`
}

func (h *handlers) recordFinding(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil { // R1-API-06: role before configuration
		return nil, err
	}
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"finding", "evidence_ref"}, nil); err != nil {
		return nil, err
	}
	finding, err := text(o, "finding", 2000)
	if err != nil {
		return nil, err
	}
	ev, err := text(o, "evidence_ref", 512)
	if err != nil {
		return nil, err
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	seq, err := h.e.RecordHeldFinding(ctx, p, ref, finding, ev)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusCreated, findingResponse{OpRef: ref, AnchorSeq: seq}}, nil
}

type repairResponse struct {
	OpRef    string `json:"op_ref"`
	RepairID string `json:"repair_id"`
	Outcome  string `json:"outcome,omitempty"`
	Approved *bool  `json:"approved,omitempty"`
}

func (h *handlers) prepareRepair(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil { // R1-API-06: role before configuration
		return nil, err
	}
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"outcome", "evidence_ref"}, []string{"summary"}); err != nil {
		return nil, err
	}
	outcome, _ := o["outcome"].(string)
	if outcome != engine.RepairCommit && outcome != engine.RepairRelease {
		return nil, bad("SCHEMA_VIOLATION", "outcome must be COMMIT or RELEASE")
	}
	ev, err := text(o, "evidence_ref", 512)
	if err != nil {
		return nil, err
	}
	var summary string
	if _, present := o["summary"]; present {
		if summary, err = text(o, "summary", 2000); err != nil {
			return nil, err
		}
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	id, err := h.e.PrepareRepair(ctx, p, ref, outcome, ev, summary)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusCreated, repairResponse{OpRef: ref, RepairID: id, Outcome: outcome}}, nil
}

func repairID(r *http.Request) (string, error) {
	id := r.PathValue("repair_id")
	if len(id) > 128 || !identifier.MatchString(id) {
		return "", bad("NOT_FOUND", "repair not found")
	}
	return id, nil
}

func (h *handlers) reviewRepair(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil { // R1-API-06: role before configuration
		return nil, err
	}
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"approved"}, []string{"note"}); err != nil {
		return nil, err
	}
	approved, ok := o["approved"].(bool)
	if !ok {
		return nil, bad("SCHEMA_VIOLATION", "approved must be a boolean")
	}
	var note string
	if _, present := o["note"]; present {
		if note, err = text(o, "note", 2000); err != nil {
			return nil, err
		}
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	id, err := repairID(r)
	if err != nil {
		return nil, err
	}
	if err := h.e.ReviewRepair(ctx, p, ref, id, approved, note); err != nil {
		return nil, err
	}
	return &response{http.StatusOK, repairResponse{OpRef: ref, RepairID: id, Approved: &approved}}, nil
}

func (h *handlers) confirmRepair(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil { // R1-API-06: role before configuration
		return nil, err
	}
	if err := emptyBody(r); err != nil {
		return nil, err
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	id, err := repairID(r)
	if err != nil {
		return nil, err
	}
	c, err := h.conn()
	if err != nil {
		return nil, err
	}
	v, err := h.e.ConfirmRepair(ctx, p, ref, id, c)
	if err != nil {
		return nil, err
	}
	return &response{http.StatusOK, v}, nil
}

type anchorRepairResponse struct {
	OpRef    string `json:"op_ref"`
	Repaired bool   `json:"repaired"`
}

func (h *handlers) anchorRepair(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Operator); err != nil { // R1-API-06: role before configuration
		return nil, err
	}
	o, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := closed(o, "", []string{"evidence_ref"}, nil); err != nil {
		return nil, err
	}
	ev, err := text(o, "evidence_ref", 512)
	if err != nil {
		return nil, err
	}
	ref, err := opRef(r)
	if err != nil {
		return nil, err
	}
	if err := h.e.ConfirmAnchorRepair(ctx, p, ref, ev); err != nil {
		return nil, err
	}
	return &response{http.StatusOK, anchorRepairResponse{OpRef: ref, Repaired: true}}, nil
}

func (h *handlers) writeState(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	if err := requireKind(p, auth.Observer, auth.Operator); err != nil {
		return nil, err
	}
	if h.e.Anchor == nil {
		return nil, bad("SERVICE_UNAVAILABLE", "no recovery anchor is configured")
	}
	ws, err := h.e.Anchor.WriteState(ctx)
	if err != nil {
		return nil, bad("SERVICE_UNAVAILABLE", "recovery anchor unavailable")
	}
	return &response{http.StatusOK, ws}, nil
}

func (h *handlers) recoveryCall(p auth.Principal, r *http.Request) (connector.Connector, error) {
	if err := requireKind(p, auth.Operator); err != nil {
		return nil, err
	}
	if err := emptyBody(r); err != nil {
		return nil, err
	}
	return h.conn()
}

func (h *handlers) checkRestore(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	c, err := h.recoveryCall(p, r)
	if err != nil {
		return nil, err
	}
	v, err := h.e.CheckRestore(ctx, c)
	if err != nil {
		return nil, err
	}
	if v.Signals == nil {
		v.Signals = []engine.RestoreSignal{}
	}
	v.Flagged, v.Unconfirmed = orEmpty(v.Flagged), orEmpty(v.Unconfirmed)
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) reconcileRestore(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	c, err := h.recoveryCall(p, r)
	if err != nil {
		return nil, err
	}
	v, err := h.e.ReconcileRestore(ctx, c)
	if err != nil {
		return nil, err
	}
	v.Recreated, v.FromOdoo, v.Flagged = orEmpty(v.Recreated), orEmpty(v.FromOdoo), orEmpty(v.Flagged)
	v.Refenced, v.Settled, v.Problems = orEmpty(v.Refenced), orEmpty(v.Settled), orEmpty(v.Problems)
	v.Unconfirmed = orEmpty(v.Unconfirmed)
	return &response{http.StatusOK, v}, nil
}

func (h *handlers) resume(ctx context.Context, p auth.Principal, r *http.Request) (*response, error) {
	c, err := h.recoveryCall(p, r)
	if err != nil {
		return nil, err
	}
	v, err := h.e.TryResume(ctx, c)
	if err != nil {
		return nil, err
	}
	if v.Checks == nil {
		v.Checks = []engine.GateCheck{}
	}
	return &response{http.StatusOK, v}, nil
}
