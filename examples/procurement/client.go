// Package procurement runs the R0 CLI demonstration scenarios against a
// running Steelmatter service (I08). All data is synthetic; this is a
// simulation, not a connection to any real ERP, bank or supplier.
//
// The runner talks to the real HTTP API with synthetic principal tokens. An
// administrator database connection is used only for trusted operator
// configuration (workflow assignments), the out-of-band supplier change in
// the stale-supplier scenario, and read-only ledger display.
package procurement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Env is everything a scenario needs.
type Env struct {
	BaseURL string
	Tokens  map[string]string // principal id → token
	Admin   *pgxpool.Pool     // database administrator (operator configuration and display only)
	Tenant  string
	RunID   string // makes source record IDs unique per run
	HTTP    *http.Client
	Out     io.Writer
	// EvidenceDir, if set, receives the flagship evidence bundle and trust store path hints.
	EvidenceDir string
	// FixtureRoot is the repository root that scenario fixture paths are
	// relative to ("" = current directory).
	FixtureRoot string
}

const (
	SourceA = "synthetic-erp"
	SourceB = "synthetic-erp-b"
	tenLakh = int64(100_000_000) // INR 10,00,000 in paise
	sixLakh = int64(60_000_000)  // INR 6,00,000
)

type resp struct {
	Status int
	Body   map[string]any
	Err    error
}

func (r resp) code() string {
	if r.Body == nil {
		return ""
	}
	if c, ok := r.Body["error_code"].(string); ok {
		return c
	}
	if c, ok := r.Body["reason"].(string); ok {
		return c
	}
	return ""
}

func (e *Env) client() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (e *Env) call(ctx context.Context, base, method, path, who string, body any) resp {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{Err: err}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return resp{Err: err}
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+e.Tokens[who])
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.client().Do(req)
	if err != nil {
		return resp{Err: err}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&out.Body); err != nil {
			out.Err = fmt.Errorf("non-JSON response: %s", raw)
		}
	}
	return out
}

func (e *Env) do(ctx context.Context, method, path, who string, body any) resp {
	return e.call(ctx, e.BaseURL, method, path, who, body)
}

// INR formats paise as rupees with Indian digit grouping, e.g. 10,00,000.
func INR(paise int64) string {
	neg := paise < 0
	if neg {
		paise = -paise
	}
	rupees, p := paise/100, paise%100
	s := fmt.Sprint(rupees)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var groups []string
		for len(head) > 2 {
			groups = append([]string{head[len(head)-2:]}, groups...)
			head = head[:len(head)-2]
		}
		if head != "" {
			groups = append([]string{head}, groups...)
		}
		s = strings.Join(groups, ",") + "," + tail
	}
	if p != 0 {
		s += fmt.Sprintf(".%02d", p)
	}
	if neg {
		s = "-" + s
	}
	return "INR " + s
}

// Ledger is the allowance state of one mandate plus its target orders.
type Ledger struct {
	Limit, Available, Reserved, Committed, Unresolved int64
	Orders                                            int
}

func (e *Env) ledger(ctx context.Context, mandateID string) (Ledger, error) {
	var l Ledger
	err := e.Admin.QueryRow(ctx, `SELECT limit_minor, available_minor, reserved_minor, committed_minor, unresolved_minor,
		(SELECT count(*) FROM target.purchase_orders o JOIN steelmatter.instructions i
		   ON i.tenant_id = o.tenant_id AND i.instruction_id = o.instruction_id WHERE i.mandate_id = m.mandate_id)
		FROM steelmatter.mandates m WHERE mandate_id = $1`, mandateID).
		Scan(&l.Limit, &l.Available, &l.Reserved, &l.Committed, &l.Unresolved, &l.Orders)
	return l, err
}

func (l Ledger) String() string {
	return fmt.Sprintf("available %s | reserved %s | committed %s | unresolved %s | orders %d",
		INR(l.Available), INR(l.Reserved), INR(l.Committed), INR(l.Unresolved), l.Orders)
}

// Detail renders the underlying minor units for developer detail.
func (l Ledger) Detail() string {
	return fmt.Sprintf("paise: limit=%d available=%d reserved=%d committed=%d unresolved=%d", l.Limit, l.Available, l.Reserved, l.Committed, l.Unresolved)
}
