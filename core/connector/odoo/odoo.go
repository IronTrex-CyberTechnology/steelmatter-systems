// Package odoo is the real connector.Connector for an Odoo 18 destination
// running the steelmatter_r1 module (destinations/odoo/addons). It calls the
// module's steelmatter.api methods over Odoo's JSON-RPC with two separate
// identities:
//
//   - executor: sm_create, sm_fence and sm_readiness only. In Odoo it has no
//     purchase rights of its own; it can write only through the module, which
//     accepts only the canonical action.
//   - observer: read-only. sm_lookup, sm_list_since, sm_observe and the
//     supplier revision read. It is never used for a write.
//
// The rule this package keeps (D1 v1.2): a transport failure, timeout,
// malformed reply or Odoo error is never proof of anything. Execute and Fence
// report UNKNOWN, Lookup reports UNAVAILABLE, Readiness reports NOT_READY.
// CREATED and FOUND are reported only with the destination's order facts
// behind them. The connector never retries a write on its own; the engine
// schedules every retry with the same op_ref.
//
// Credentials are never put in results, errors or logs.
package odoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
)

// Credentials is one Odoo login. Password may be an Odoo API key.
type Credentials struct {
	Login    string
	Password string
}

// Config configures a Connector.
type Config struct {
	// URL is the Odoo base URL, for example https://odoo.example:8069. Plain
	// http is accepted only for a loopback host (the local sandbox).
	URL      string
	Database string
	Executor Credentials
	Observer Credentials
	// Timeout bounds each call. A call that times out after it was sent is
	// UNKNOWN, never "not sent". Default 30s.
	Timeout time.Duration
	// Instance names this connector in each result's source. Default "odoo".
	Instance string
	// HTTPClient is optional; its own Timeout is not used (see Timeout).
	HTTPClient *http.Client
}

// Reason codes this package adds for failures it detects itself.
const (
	ReasonMalformedReply   = "MALFORMED_REPLY"
	ReasonDestinationError = "DESTINATION_ERROR"
	ReasonMissingFacts     = "MISSING_DESTINATION_FACTS"
)

// SupportedSeries is the Odoo release series this connector is built and
// tested against (D7: Odoo 18 self-hosted, proposed).
const SupportedSeries = "18.0"

type identity int

const (
	executor identity = iota
	observer
)

func (i identity) String() string {
	if i == executor {
		return "executor"
	}
	return "observer"
}

// Connector is safe for concurrent use.
type Connector struct {
	endpoint string
	db       string
	timeout  time.Duration
	instance string
	client   *http.Client
	nextID   atomic.Int64

	mu    sync.Mutex
	creds [2]Credentials
	uids  [2]int64 // 0 = not logged in yet
}

var _ connector.Connector = (*Connector)(nil)

// New returns a Connector. It does not contact Odoo.
func New(cfg Config) (*Connector, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("odoo: URL must be an http(s) URL with a host")
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return nil, errors.New("odoo: plain http is allowed only for a loopback host; use https")
	}
	if cfg.Database == "" {
		return nil, errors.New("odoo: Database is required")
	}
	if cfg.Executor.Login == "" || cfg.Observer.Login == "" || cfg.Executor.Password == "" || cfg.Observer.Password == "" {
		return nil, errors.New("odoo: executor and observer credentials are required")
	}
	if cfg.Executor.Login == cfg.Observer.Login {
		return nil, errors.New("odoo: the executor and observer must be separate identities")
	}
	c := &Connector{
		endpoint: strings.TrimRight(u.String(), "/") + "/jsonrpc",
		db:       cfg.Database,
		timeout:  cfg.Timeout,
		instance: cfg.Instance,
		client:   cfg.HTTPClient,
		creds:    [2]Credentials{cfg.Executor, cfg.Observer},
	}
	if c.timeout <= 0 {
		c.timeout = 30 * time.Second
	}
	if c.instance == "" {
		c.instance = "odoo"
	}
	// Never follow a redirect (R1-DEF-16): every request body carries a
	// password, and a 307/308 would resend it to whatever host the reply
	// names. A redirect is reported like any other non-200 reply: UNKNOWN for
	// writes, never proof. A caller's client is copied, not changed.
	client := http.Client{}
	if c.client != nil {
		client = *c.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.client = &client
	return c, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SetCredentials rotates both identities. Calls already sent keep the old
// credentials; later calls log in again with the new ones.
func (c *Connector) SetCredentials(exec, obs Credentials) error {
	if exec.Login == "" || obs.Login == "" || exec.Password == "" || obs.Password == "" || exec.Login == obs.Login {
		return errors.New("odoo: separate executor and observer credentials are required")
	}
	c.mu.Lock()
	c.creds, c.uids = [2]Credentials{exec, obs}, [2]int64{}
	c.mu.Unlock()
	return nil
}

// ------------------------------------------------------------------ transport

// callError classifies a failed call. Its text never contains credentials.
type callError struct {
	reason string // stable reason code
	detail string // Odoo exception name or transport error class
}

func (e *callError) Error() string { return "odoo: " + e.reason + ": " + e.detail }

func reasonOf(err error) string {
	var ce *callError
	if errors.As(err, &ce) {
		return ce.reason
	}
	return connector.ReasonDestinationUnavailable
}

type rpcRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	Params  rpcParams `json:"params"`
	ID      int64     `json:"id"`
}

type rpcParams struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Args    []any  `json:"args"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    struct {
			Name string `json:"name"`
		} `json:"data"`
	} `json:"error"`
}

func (c *Connector) rpc(ctx context.Context, service, method string, args []any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "call", Params: rpcParams{service, method, args}, ID: c.nextID.Add(1)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return &callError{connector.ReasonDestinationUnavailable, transportClass(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &callError{connector.ReasonDestinationUnavailable, transportClass(err)}
	}
	if resp.StatusCode != http.StatusOK {
		return &callError{connector.ReasonDestinationUnavailable, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return &callError{ReasonMalformedReply, "reply is not JSON-RPC"}
	}
	if r.Error != nil {
		name := r.Error.Data.Name
		if name == "" {
			name = "odoo error"
		}
		if strings.HasSuffix(name, "AccessDenied") {
			return &callError{connector.ReasonCredentialDisabled, name}
		}
		return &callError{ReasonDestinationError, name}
	}
	if out == nil {
		return nil
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return &callError{ReasonMalformedReply, "empty result"}
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return &callError{ReasonMalformedReply, "unexpected result shape"}
	}
	return nil
}

func transportClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "transport error"
	}
}

// login returns the identity's uid, logging in once per credential set.
func (c *Connector) login(ctx context.Context, who identity) (int64, Credentials, error) {
	c.mu.Lock()
	uid, cr := c.uids[who], c.creds[who]
	c.mu.Unlock()
	if uid != 0 {
		return uid, cr, nil
	}
	var got any
	if err := c.rpc(ctx, "common", "login", []any{c.db, cr.Login, cr.Password}, &got); err != nil {
		return 0, cr, err
	}
	n, ok := got.(float64)
	if !ok || n <= 0 {
		return 0, cr, &callError{connector.ReasonCredentialDisabled, who.String() + " login refused"}
	}
	c.mu.Lock()
	if c.creds[who] == cr {
		c.uids[who] = int64(n)
	}
	c.mu.Unlock()
	return int64(n), cr, nil
}

// call runs model.method as who.
func (c *Connector) call(ctx context.Context, who identity, model, method string, args []any, kw map[string]any, out any) error {
	uid, cr, err := c.login(ctx, who)
	if err != nil {
		return err
	}
	if args == nil {
		args = []any{}
	}
	if kw == nil {
		kw = map[string]any{}
	}
	err = c.rpc(ctx, "object", "execute_kw", []any{c.db, uid, cr.Password, model, method, args, kw}, out)
	if reasonOf(err) == connector.ReasonCredentialDisabled {
		c.mu.Lock()
		if c.creds[who] == cr {
			c.uids[who] = 0 // log in again next time (rotation, disabled user)
		}
		c.mu.Unlock()
	}
	return err
}

func (c *Connector) api(ctx context.Context, who identity, method string, args ...any) (connector.Result, error) {
	var r connector.Result
	err := c.call(ctx, who, "steelmatter.api", method, args, nil, &r)
	return r, err
}

// ------------------------------------------------------------------ results

// finish checks a decoded module result and fills in connector fields. A
// reply that does not match the request, or that claims an order without
// its facts, is turned into failOutcome: never into a success or a proof.
func (c *Connector) finish(r connector.Result, err error, opRef, call string, who identity, failOutcome connector.Outcome) connector.Result {
	now := time.Now().UTC()
	fail := func(reason string) connector.Result {
		return connector.Result{OpRef: opRef, Call: call, Outcome: failOutcome, Reason: reason, ConnectorReceivedAt: now,
			Source: c.instance + "/" + who.String()}
	}
	if err != nil {
		return fail(reasonOf(err))
	}
	if r.OpRef != opRef || r.Call != call || r.Outcome == "" {
		return fail(ReasonMalformedReply)
	}
	if (r.Outcome == connector.Created || r.Outcome == connector.Found) && r.Destination == nil {
		return fail(ReasonMissingFacts)
	}
	if r.CreateDone && r.Destination == nil {
		r.CreateDone = false // a CREATE_DONE claim needs its order facts
	}
	r.ConnectorReceivedAt = now
	r.Source = c.instance + "/" + who.String() + " " + r.Source
	return r
}

// ------------------------------------------------------------------ calls

// Execute implements connector.Connector (sm_create, executor identity).
func (c *Connector) Execute(ctx context.Context, a connector.Action) (connector.Result, error) {
	if a.OpRef == "" {
		return connector.Result{}, errors.New("odoo: Execute needs an op_ref")
	}
	r, err := c.api(ctx, executor, "sm_create", a)
	return c.finish(r, err, a.OpRef, "execute", executor, connector.Unknown), nil
}

// Lookup implements connector.Connector (sm_lookup, observer identity: a read).
func (c *Connector) Lookup(ctx context.Context, opRef string) (connector.Result, error) {
	if opRef == "" {
		return connector.Result{}, errors.New("odoo: Lookup needs an op_ref")
	}
	r, err := c.api(ctx, observer, "sm_lookup", opRef)
	return c.finish(r, err, opRef, "lookup", observer, connector.Unavailable), nil
}

// Fence implements connector.Connector (sm_fence, executor identity).
func (c *Connector) Fence(ctx context.Context, opRef string, expectedMinSeq int64) (connector.Result, error) {
	if opRef == "" {
		return connector.Result{}, errors.New("odoo: Fence needs an op_ref")
	}
	r, err := c.api(ctx, executor, "sm_fence", opRef, expectedMinSeq)
	return c.finish(r, err, opRef, "fence", executor, connector.Unknown), nil
}

// Readiness implements connector.Connector. Advisory only: it checks the
// Odoo version, the mappings, the supplier revision and the anchor, but
// READY never proves anything about the later write.
func (c *Connector) Readiness(ctx context.Context, a connector.Action) (connector.Result, error) {
	now := time.Now().UTC()
	notReady := func(reason string) (connector.Result, error) {
		return connector.Result{OpRef: a.OpRef, Call: "readiness", Outcome: connector.NotReady, Reason: reason,
			ConnectorReceivedAt: now, Source: c.instance + "/executor"}, nil
	}
	var v struct {
		Module    string `json:"module"`
		Odoo      string `json:"odoo"`
		Supported bool   `json:"supported"`
	}
	if err := c.call(ctx, executor, "steelmatter.api", "sm_version", nil, nil, &v); err != nil {
		return notReady(reasonOf(err))
	}
	if !v.Supported || !strings.HasPrefix(v.Odoo, SupportedSeries) {
		return notReady(connector.ReasonVersionUnsupported)
	}
	r, err := c.api(ctx, executor, "sm_readiness", a)
	if err != nil {
		return notReady(reasonOf(err))
	}
	if r.Outcome != connector.Ready && r.Outcome != connector.NotReady {
		return notReady(ReasonMalformedReply)
	}
	r.ConnectorReceivedAt = now
	r.Source = c.instance + "/executor " + r.Source
	return r, nil
}

// Observe implements connector.Connector (sm_observe, observer identity).
// Evidence only; a failure is INSUFFICIENT_EVIDENCE.
func (c *Connector) Observe(ctx context.Context, a connector.Action) (connector.Result, error) {
	r, err := c.api(ctx, observer, "sm_observe", a)
	if err == nil && r.Outcome != connector.Match && r.Outcome != connector.Mismatch &&
		r.Outcome != connector.InsufficientEvidence && r.Outcome != connector.NotFound {
		err = &callError{ReasonMalformedReply, "unexpected observe outcome"}
	}
	return c.finish(r, err, a.OpRef, "observe", observer, connector.InsufficientEvidence), nil
}

// List implements connector.Connector (sm_list_since, observer identity).
func (c *Connector) List(ctx context.Context, checkpointSeq int64, limit int) (connector.ListPage, error) {
	var p connector.ListPage
	if err := c.call(ctx, observer, "steelmatter.api", "sm_list_since", []any{checkpointSeq, limit}, nil, &p); err != nil {
		return connector.ListPage{}, fmt.Errorf("destination list: %w", err)
	}
	prev := checkpointSeq
	for _, row := range p.Rows {
		if row.Seq <= prev {
			return connector.ListPage{}, fmt.Errorf("destination list: %w", &callError{ReasonMalformedReply, "rows out of commit order"})
		}
		prev = row.Seq
	}
	return p, nil
}

// SupplierRevision reads the destination's protected supplier revision
// counter (res.partner.sm_revision, D1 v1.1) for supplierRef with the
// read-only observer identity, for the engine to store when it authorises
// an action. The destination compares it again inside the create.
func (c *Connector) SupplierRevision(ctx context.Context, supplierRef string) (int64, error) {
	var rows []struct {
		Rev int64 `json:"sm_revision"`
	}
	err := c.call(ctx, observer, "res.partner", "search_read",
		[]any{[]any{[]any{"sm_ref", "=", supplierRef}}}, map[string]any{"fields": []string{"sm_revision"}, "limit": 2}, &rows)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("odoo: supplier %q: %s", supplierRef, connector.ReasonMappingMissing)
	}
	return rows[0].Rev, nil
}
