package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/schemas"
)

// Kind names one of the five payload types.
type Kind string

const (
	KindMandate     Kind = "mandate"
	KindAction      Kind = "action"
	KindPermit      Kind = "permit"
	KindExecution   Kind = "execution"
	KindObservation Kind = "observation"
)

// Kinds lists every payload kind.
var Kinds = []Kind{KindMandate, KindAction, KindPermit, KindExecution, KindObservation}

const (
	SchemaVersion = "1"
	CryptoProfile = "SM-JWS-ES256-1"
	Currency      = "INR"
	// MaxPermitLifetime is the 5-minute permit limit (document 02).
	MaxPermitLifetime = 5 * time.Minute
	timestampLayout   = "2006-01-02T15:04:05Z"
)

// Mandate is the owner's bounded permission.
type Mandate struct {
	SchemaVersion string   `json:"schema_version"`
	CryptoProfile string   `json:"crypto_profile"`
	MandateID     string   `json:"mandate_id"`
	TenantID      string   `json:"tenant_id"`
	IssuerID      string   `json:"issuer_id"`
	Revision      int64    `json:"revision"`
	ControlEpoch  int64    `json:"control_epoch"`
	ActorIDs      []string `json:"actor_ids"`
	Audience      string   `json:"audience"`
	Operation     string   `json:"operation"`
	SupplierIDs   []string `json:"supplier_ids"`
	DeliveryIDs   []string `json:"delivery_ids"`
	Currency      string   `json:"currency"`
	LimitMinor    int64    `json:"limit_minor"`
	IssuedAt      string   `json:"issued_at"`
	ExpiresAt     string   `json:"expires_at"`
}

// Line is one purchase-order line.
type Line struct {
	ItemID         string `json:"item_id"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

// Action is the exact proposed purchase order. It is digested, not signed;
// the permit binds its digest. It never carries an obligation_id (T39).
type Action struct {
	SchemaVersion    string `json:"schema_version"`
	CryptoProfile    string `json:"crypto_profile"`
	TenantID         string `json:"tenant_id"`
	InstructionID    string `json:"instruction_id"`
	ActionRevision   int64  `json:"action_revision"`
	ActorID          string `json:"actor_id"`
	Audience         string `json:"audience"`
	Operation        string `json:"operation"`
	SupplierID       string `json:"supplier_id"`
	SupplierRevision int64  `json:"supplier_revision"`
	DeliveryID       string `json:"delivery_id"`
	Currency         string `json:"currency"`
	Lines            []Line `json:"lines"`
	AmountMinor      int64  `json:"amount_minor"`
}

// Permit is the signed, time-limited permission for one action revision.
type Permit struct {
	SchemaVersion   string `json:"schema_version"`
	CryptoProfile   string `json:"crypto_profile"`
	PermitID        string `json:"permit_id"`
	ReservationID   string `json:"reservation_id"`
	OperationID     string `json:"operation_id"`
	TenantID        string `json:"tenant_id"`
	InstructionID   string `json:"instruction_id"`
	ActionRevision  int64  `json:"action_revision"`
	ActionDigest    string `json:"action_digest"`
	MandateID       string `json:"mandate_id"`
	MandateRevision int64  `json:"mandate_revision"`
	MandateDigest   string `json:"mandate_digest"`
	ControlEpoch    int64  `json:"control_epoch"`
	ActorID         string `json:"actor_id"`
	Audience        string `json:"audience"`
	Currency        string `json:"currency"`
	AmountMinor     int64  `json:"amount_minor"`
	IssuedAt        string `json:"issued_at"`
	ExpiresAt       string `json:"expires_at"`
}

// Execution is the executor's statement of what it committed.
type Execution struct {
	SchemaVersion string `json:"schema_version"`
	CryptoProfile string `json:"crypto_profile"`
	ExecutionID   string `json:"execution_id"`
	OperationID   string `json:"operation_id"`
	TenantID      string `json:"tenant_id"`
	InstructionID string `json:"instruction_id"`
	ObligationID  string `json:"obligation_id"`
	ActionDigest  string `json:"action_digest"`
	PermitDigest  string `json:"permit_digest"`
	TargetOrderID string `json:"target_order_id"`
	Currency      string `json:"currency"`
	AmountMinor   int64  `json:"amount_minor"`
	Status        string `json:"status"`
	CommittedAt   string `json:"committed_at"`
}

// Observation is the observer's readback comparison.
type Observation struct {
	SchemaVersion        string  `json:"schema_version"`
	CryptoProfile        string  `json:"crypto_profile"`
	ObservationID        string  `json:"observation_id"`
	ObserverID           string  `json:"observer_id"`
	OperationID          string  `json:"operation_id"`
	TenantID             string  `json:"tenant_id"`
	InstructionID        string  `json:"instruction_id"`
	ExecutionDigest      string  `json:"execution_digest"`
	ExpectedActionDigest string  `json:"expected_action_digest"`
	ObservedActionDigest *string `json:"observed_action_digest"`
	TargetOrderID        string  `json:"target_order_id"`
	Status               string  `json:"status"`
	ObservedAt           string  `json:"observed_at"`
}

// Timestamp fields per kind, checked before schema validation.
var timestampFields = map[Kind][]string{
	KindMandate:     {"issued_at", "expires_at"},
	KindPermit:      {"issued_at", "expires_at"},
	KindExecution:   {"committed_at"},
	KindObservation: {"observed_at"},
}

var (
	compileOnce sync.Once
	compiled    map[Kind]*jsonschema.Schema
	compileErr  error
)

func schemaFor(k Kind) (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		compiled = map[Kind]*jsonschema.Schema{}
		for _, kind := range Kinds {
			name := string(kind) + ".schema.json"
			raw, err := schemas.FS.ReadFile(name)
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				compileErr = err
				return
			}
			if err := c.AddResource(name, doc); err != nil {
				compileErr = err
				return
			}
			s, err := c.Compile(name)
			if err != nil {
				compileErr = err
				return
			}
			compiled[kind] = s
		}
	})
	if compileErr != nil {
		return nil, compileErr
	}
	s, ok := compiled[k]
	if !ok {
		return nil, fmt.Errorf("unknown payload kind %q", k)
	}
	return s, nil
}

// knownFields returns the closed property set for an object schema.
var knownFields = map[Kind]map[string]bool{}
var lineFields = map[string]bool{"item_id": true, "quantity": true, "unit_price_minor": true}

func init() {
	for k, v := range map[Kind]any{KindMandate: Mandate{}, KindAction: Action{}, KindPermit: Permit{}, KindExecution: Execution{}, KindObservation: Observation{}} {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		knownFields[k] = map[string]bool{}
		for f := range m {
			knownFields[k][f] = true
		}
	}
}

// Validate parses raw strictly and validates it as kind, returning the typed
// payload (*Mandate, *Action, *Permit, *Execution or *Observation).
//
// Check order (pinned, mirrored by the verifier): strict parse; top-level
// object; schema_version; crypto_profile; unknown fields; currency;
// timestamps; JSON Schema; semantic rules.
func Validate(kind Kind, raw []byte) (any, *Error) {
	tree, perr := ParseStrict(raw)
	if perr != nil {
		return nil, perr
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return nil, errf(CodeSchemaViolation, "", "payload must be a JSON object")
	}
	if v, ok := obj["schema_version"].(string); !ok || v != SchemaVersion {
		return nil, errf(CodeUnsupportedVersion, "/schema_version", "schema_version must be %q", SchemaVersion)
	}
	if v, ok := obj["crypto_profile"].(string); !ok || v != CryptoProfile {
		return nil, errf(CodeUnsupportedProfile, "/crypto_profile", "crypto_profile must be %q", CryptoProfile)
	}
	fields, ok := knownFields[kind]
	if !ok {
		return nil, errf(CodeSchemaViolation, "", "unknown payload kind %q", kind)
	}
	for _, f := range sortedKeys(obj) {
		if !fields[f] {
			return nil, errf(CodeUnknownField, "/"+escapePointer(f), "field %q is not part of the %s payload", f, kind)
		}
	}
	if kind == KindAction {
		if lines, ok := obj["lines"].([]any); ok {
			for i, l := range lines {
				if lo, ok := l.(map[string]any); ok {
					for _, f := range sortedKeys(lo) {
						if !lineFields[f] {
							return nil, errf(CodeUnknownField, fmt.Sprintf("/lines/%d/%s", i, escapePointer(f)), "field %q is not part of a line", f)
						}
					}
				}
			}
		}
	}
	if c, present := obj["currency"]; present {
		if s, ok := c.(string); !ok || s != Currency {
			return nil, errf(CodeUnsupportedCurrency, "/currency", "only %s is supported", Currency)
		}
	}
	for _, f := range timestampFields[kind] {
		if s, ok := obj[f].(string); ok {
			if _, err := ParseTimestamp(s); err != nil {
				return nil, errf(CodeInvalidTimestamp, "/"+f, "%v", err)
			}
		}
	}
	sch, err := schemaFor(kind)
	if err != nil {
		return nil, errf(CodeSchemaViolation, "", "schema unavailable: %v", err)
	}
	if err := sch.Validate(tree); err != nil {
		return nil, errf(CodeSchemaViolation, "", "%s", firstSchemaError(err))
	}

	var out any
	switch kind {
	case KindMandate:
		out = &Mandate{}
	case KindAction:
		out = &Action{}
	case KindPermit:
		out = &Permit{}
	case KindExecution:
		out = &Execution{}
	case KindObservation:
		out = &Observation{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return nil, errf(CodeSchemaViolation, "", "decode: %v", err)
	}
	if e := checkSemantics(out); e != nil {
		return nil, e
	}
	return out, nil
}

// ParseTimestamp accepts only real UTC calendar times in whole seconds with a
// Z suffix: YYYY-MM-DDTHH:MM:SSZ. Leap seconds (:60) are rejected.
func ParseTimestamp(s string) (time.Time, error) {
	if len(s) != len(timestampLayout) {
		return time.Time{}, fmt.Errorf("timestamp %q must be YYYY-MM-DDTHH:MM:SSZ", s)
	}
	t, err := time.Parse(timestampLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not a real UTC time: %v", s, err)
	}
	if t.Format(timestampLayout) != s {
		return time.Time{}, fmt.Errorf("timestamp %q does not round-trip", s)
	}
	return t, nil
}

func checkSemantics(p any) *Error {
	switch v := p.(type) {
	case *Action:
		return checkLines(v.Lines, v.AmountMinor)
	case *Mandate:
		return checkRange(v.IssuedAt, v.ExpiresAt, 0)
	case *Permit:
		return checkRange(v.IssuedAt, v.ExpiresAt, MaxPermitLifetime)
	case *Observation:
		return checkObservation(v)
	}
	return nil
}

// checkLines enforces unique item IDs, ascending byte order and
// amount_minor = Σ quantity × unit_price_minor with checked arithmetic.
// Signed or stored actions must already be canonical; NormaliseLines is the
// only place proposal order is corrected.
func checkLines(lines []Line, amount int64) *Error {
	seen := map[string]bool{}
	for i, l := range lines {
		if seen[l.ItemID] {
			return errf(CodeDuplicateLineItem, fmt.Sprintf("/lines/%d/item_id", i), "item_id %q appears more than once", l.ItemID)
		}
		seen[l.ItemID] = true
	}
	for i := 1; i < len(lines); i++ {
		if lines[i-1].ItemID >= lines[i].ItemID {
			return errf(CodeLinesNotCanonical, fmt.Sprintf("/lines/%d", i), "lines must be sorted by item_id")
		}
	}
	total, err := LineTotal(lines)
	if err != nil {
		return err
	}
	if total != amount {
		return errf(CodeAmountMismatch, "/amount_minor", "amount_minor %d does not equal line total %d", amount, total)
	}
	return nil
}

// LineTotal returns Σ quantity × unit_price_minor, rejecting any intermediate
// value above 2^53-1.
func LineTotal(lines []Line) (int64, *Error) {
	var total int64
	for i, l := range lines {
		if l.Quantity < 0 || l.UnitPriceMinor < 0 {
			return 0, errf(CodeSchemaViolation, fmt.Sprintf("/lines/%d", i), "negative quantity or price")
		}
		if l.UnitPriceMinor != 0 && l.Quantity > MaxSafeInteger/l.UnitPriceMinor {
			return 0, errf(CodeArithmeticOverflow, fmt.Sprintf("/lines/%d", i), "quantity × unit_price_minor exceeds 2^53-1")
		}
		product := l.Quantity * l.UnitPriceMinor
		if total > MaxSafeInteger-product {
			return 0, errf(CodeArithmeticOverflow, "/lines", "line total exceeds 2^53-1")
		}
		total += product
	}
	return total, nil
}

// NormaliseLines prepares issuer-proposed lines before authorisation: it
// rejects duplicate item IDs (never merges) and sorts by item_id bytes.
func NormaliseLines(lines []Line) ([]Line, *Error) {
	seen := map[string]bool{}
	out := make([]Line, len(lines))
	for i, l := range lines {
		if seen[l.ItemID] {
			return nil, errf(CodeDuplicateLineItem, fmt.Sprintf("/lines/%d/item_id", i), "item_id %q appears more than once", l.ItemID)
		}
		seen[l.ItemID] = true
		out[i] = l
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out, nil
}

func checkRange(issued, expires string, maxLifetime time.Duration) *Error {
	i, _ := ParseTimestamp(issued)
	e, _ := ParseTimestamp(expires)
	if !e.After(i) {
		return errf(CodeInvalidTimeRange, "/expires_at", "expires_at must be after issued_at")
	}
	if maxLifetime > 0 && e.Sub(i) > maxLifetime {
		return errf(CodeInvalidTimeRange, "/expires_at", "lifetime %s exceeds %s", e.Sub(i), maxLifetime)
	}
	return nil
}

// checkObservation pins status/digest consistency: MATCH requires an observed
// digest equal to the expected one; MISMATCH a different observed digest;
// INSUFFICIENT_EVIDENCE no observed digest.
func checkObservation(o *Observation) *Error {
	switch o.Status {
	case "MATCH":
		if o.ObservedActionDigest == nil || *o.ObservedActionDigest != o.ExpectedActionDigest {
			return errf(CodeObservationInconsistent, "/status", "MATCH requires observed_action_digest equal to expected_action_digest")
		}
	case "MISMATCH":
		if o.ObservedActionDigest == nil || *o.ObservedActionDigest == o.ExpectedActionDigest {
			return errf(CodeObservationInconsistent, "/status", "MISMATCH requires a different observed_action_digest")
		}
	case "INSUFFICIENT_EVIDENCE":
		if o.ObservedActionDigest != nil {
			return errf(CodeObservationInconsistent, "/status", "INSUFFICIENT_EVIDENCE requires observed_action_digest null")
		}
	}
	return nil
}

func firstSchemaError(err error) string {
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		for len(ve.Causes) > 0 {
			ve = ve.Causes[0]
		}
		return ve.Error()
	}
	return err.Error()
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
