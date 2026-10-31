// Package lossproxy is a fault-injecting HTTP proxy for the Odoo JSON-RPC
// path (R1 lost-reply tests and Sindhu's QA). It sits between the connector
// and Odoo and, on command, loses traffic on the network rather than inside
// the connector:
//
//	drop_reply    forward the request, let Odoo finish (the create commits),
//	              then close the client connection without any response
//	drop_request  close the client connection without forwarding anything
//	delay_reply   forward, then hold Odoo's response for DelayMS before
//	              sending it (past the connector's timeout, it is a lost reply)
//	bad_gateway   forward, then answer 502 instead of Odoo's response
//
// A fault matches by steelmatter.api method (sm_create, sm_fence, ...) and
// optionally op_ref, and applies to the next Count matching calls. Every
// call is recorded with what Odoo actually answered, so a test can show the
// simulated failure really happened (an order was created even though the
// reply was dropped).
//
// Synthetic sandbox use only: it reads request bodies, so never put it in
// front of a real deployment.
package lossproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Mode is what a fault does to a matching call.
type Mode string

const (
	DropReply   Mode = "drop_reply"
	DropRequest Mode = "drop_request"
	DelayReply  Mode = "delay_reply"
	BadGateway  Mode = "bad_gateway"
)

// Fault applies Mode to the next Count calls that match Method and, if set,
// OpRef. Count 0 means 1.
type Fault struct {
	Mode    Mode   `json:"mode"`
	Method  string `json:"method"`
	OpRef   string `json:"op_ref,omitempty"`
	Count   int    `json:"count,omitempty"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

// Event records one proxied call.
type Event struct {
	Time            time.Time `json:"time"`
	Method          string    `json:"method"`           // steelmatter.api method, or service.method for other calls
	OpRef           string    `json:"op_ref,omitempty"` // when the call names one
	Action          string    `json:"action"`           // forwarded, or the fault mode applied
	Forwarded       bool      `json:"forwarded"`        // the request reached Odoo
	UpstreamStatus  int       `json:"upstream_status,omitempty"`
	UpstreamOutcome string    `json:"upstream_outcome,omitempty"` // Odoo's result.outcome, when it sent one
	UpstreamError   string    `json:"upstream_error,omitempty"`   // Odoo exception name, when it raised one
}

// Proxy is safe for concurrent use.
type Proxy struct {
	target *url.URL
	client *http.Client

	mu     sync.Mutex
	faults []*Fault
	events []Event
}

// New returns a proxy forwarding to target (the Odoo base URL).
func New(target string) (*Proxy, error) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("lossproxy: target must be an http(s) URL")
	}
	return &Proxy{target: u, client: &http.Client{Timeout: 10 * time.Minute}}, nil
}

// Add queues a fault.
func (p *Proxy) Add(f Fault) error {
	switch f.Mode {
	case DropReply, DropRequest, DelayReply, BadGateway:
	default:
		return fmt.Errorf("lossproxy: unknown mode %q", f.Mode)
	}
	if f.Method == "" {
		return errors.New("lossproxy: a fault needs a method, for example sm_create")
	}
	if f.Count <= 0 {
		f.Count = 1
	}
	if f.Mode == DelayReply && f.DelayMS <= 0 {
		return errors.New("lossproxy: delay_reply needs delay_ms")
	}
	p.mu.Lock()
	p.faults = append(p.faults, &f)
	p.mu.Unlock()
	return nil
}

// Reset clears pending faults and the event log.
func (p *Proxy) Reset() {
	p.mu.Lock()
	p.faults, p.events = nil, nil
	p.mu.Unlock()
}

// Pending returns the faults not yet used up.
func (p *Proxy) Pending() []Fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Fault, 0, len(p.faults))
	for _, f := range p.faults {
		out = append(out, *f)
	}
	return out
}

// Events returns the calls proxied so far, optionally only those for opRef.
func (p *Proxy) Events(opRef string) []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Event
	for _, e := range p.events {
		if opRef == "" || e.OpRef == opRef {
			out = append(out, e)
		}
	}
	return out
}

// take returns the first pending fault matching the call and uses it once.
func (p *Proxy) take(method, opRef string) *Fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, f := range p.faults {
		if f.Method == method && (f.OpRef == "" || f.OpRef == opRef) {
			c := *f
			if f.Count--; f.Count == 0 {
				p.faults = append(p.faults[:i], p.faults[i+1:]...)
			}
			return &c
		}
	}
	return nil
}

func (p *Proxy) record(e Event) {
	e.Time = time.Now().UTC()
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
}

// callOf reads the JSON-RPC body: the steelmatter.api method (or
// service.method) and the op_ref it names, if any.
func callOf(body []byte) (method, opRef string) {
	var req struct {
		Params struct {
			Service string            `json:"service"`
			Method  string            `json:"method"`
			Args    []json.RawMessage `json:"args"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "unparsed", ""
	}
	pr := req.Params
	if pr.Service != "object" || pr.Method != "execute_kw" || len(pr.Args) < 6 {
		return pr.Service + "." + pr.Method, ""
	}
	var model string
	_ = json.Unmarshal(pr.Args[3], &model)
	_ = json.Unmarshal(pr.Args[4], &method)
	if model != "steelmatter.api" {
		return model + "." + method, ""
	}
	var args []json.RawMessage
	if json.Unmarshal(pr.Args[5], &args) == nil && len(args) > 0 {
		var s string
		var a struct {
			OpRef string `json:"op_ref"`
		}
		if json.Unmarshal(args[0], &s) == nil {
			opRef = s
		} else if json.Unmarshal(args[0], &a) == nil {
			opRef = a.OpRef
		}
	}
	return method, opRef
}

func outcomeOf(body []byte) (outcome, errName string) {
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Data struct {
				Name string `json:"name"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return "", ""
	}
	if r.Error != nil {
		return "", r.Error.Data.Name
	}
	var res struct {
		Outcome string `json:"outcome"`
	}
	_ = json.Unmarshal(r.Result, &res)
	return res.Outcome, ""
}

// ServeHTTP is the data path: connector -> proxy -> Odoo.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	method, opRef := callOf(body)
	ev := Event{Method: method, OpRef: opRef, Action: "forwarded"}
	f := p.take(method, opRef)
	if f != nil {
		ev.Action = string(f.Mode)
	}
	if f != nil && f.Mode == DropRequest {
		p.record(ev)
		hangUp(w)
		return
	}

	up, err := http.NewRequestWithContext(r.Context(), r.Method, p.target.ResolveReference(&url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}).String(),
		bytes.NewReader(body))
	if err != nil {
		http.Error(w, "proxy request", http.StatusInternalServerError)
		return
	}
	up.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if f != nil && f.Mode != DelayReply {
		// Odoo must finish even though the client gets no reply.
		up = up.WithContext(context.WithoutCancel(r.Context()))
	}
	resp, err := p.client.Do(up)
	ev.Forwarded = true
	if err != nil {
		ev.UpstreamError = "transport: " + err.Error()
		p.record(ev)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	ev.UpstreamStatus = resp.StatusCode
	ev.UpstreamOutcome, ev.UpstreamError = outcomeOf(respBody)
	p.record(ev)

	switch {
	case f == nil:
	case f.Mode == DropReply:
		hangUp(w)
		return
	case f.Mode == BadGateway:
		http.Error(w, "bad gateway (lossproxy)", http.StatusBadGateway)
		return
	case f.Mode == DelayReply:
		select {
		case <-time.After(time.Duration(f.DelayMS) * time.Millisecond):
		case <-r.Context().Done():
			return // the client gave up: its reply is lost
		}
	}
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// hangUp closes the client connection without writing a response.
func hangUp(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler) // the server drops the connection
}

// Control is the fault-control API (bind it to loopback only):
//
//	POST   /faults        add a Fault (JSON body)
//	GET    /faults        pending faults
//	DELETE /faults        clear faults and events
//	GET    /events[?op_ref=X]  proxied calls and what Odoo answered
func (p *Proxy) Control() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/faults", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var f Fault
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&f); err != nil {
				http.Error(w, "body must be a fault object", http.StatusBadRequest)
				return
			}
			if err := p.Add(f); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, p.Pending())
		case http.MethodGet:
			writeJSON(w, p.Pending())
		case http.MethodDelete:
			p.Reset()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, p.Events(r.URL.Query().Get("op_ref")))
	})
	return mux
}
