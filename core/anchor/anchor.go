package anchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
)

// Engine record types.
const (
	DispatchIntent  = "DISPATCH_INTENT"
	Outcome         = "OUTCOME"
	WriteStateType  = "WRITE_STATE"
	OperatorFinding = "OPERATOR_FINDING"
	RepairEvidence  = "REPAIR_EVIDENCE"
	RepairReviewed  = "REPAIR_REVIEWED"
	RepairConfirmed = "REPAIR_CONFIRMED"
)

// Destination record types.
const (
	CreateIntent = "CREATE_INTENT"
	Block        = "BLOCK"
	CreateDone   = "CREATE_DONE"
)

// ErrWritesDisabled: writes are off, or the caller's generation is old.
var ErrWritesDisabled = errors.New("anchor: writes are disabled or the generation changed")

// ErrBlocked: the reference has a BLOCK record, so no create may start.
var ErrBlocked = errors.New("anchor: reference is permanently blocked")

// Client is the engine's connection to the anchor, using the
// anchor_engine_writer identity: it appends engine records and reads both
// streams; it cannot write destination records or change any record.
type Client struct {
	Pool *pgxpool.Pool
}

// WriteState is the anchor's write switch (WRITE_STATE).
type WriteState struct {
	Enabled     bool   `json:"enabled"`
	Generation  int64  `json:"generation"`
	Reason      string `json:"reason"`
	BaselineSeq int64  `json:"baseline_seq"`
}

// Record is one engine record.
type Record struct {
	Seq        int64
	Type       string
	Tenant     string
	OpRef      string
	Payload    json.RawMessage
	RecordedAt time.Time
}

// DestRecord is one destination record.
type DestRecord struct {
	Seq         int64
	Type        string
	Destination string
	OpRef       string
	Payload     json.RawMessage
}

// IntentRecord is the DISPATCH_INTENT payload: everything needed to rebuild
// the authorised request (verifiable against the signed permit) and the
// budget move reserved -> unresolved.
type IntentRecord struct {
	AdmittedAt       string `json:"admitted_at,omitempty"`
	Tenant           string `json:"tenant_id"`
	OpRef            string `json:"op_ref"`
	InstructionID    string `json:"instruction_id"`
	ActionRevision   int64  `json:"action_revision"`
	MandateID        string `json:"mandate_id"`
	ControlEpoch     int64  `json:"control_epoch"`
	CompanyRef       string `json:"company_ref"`
	PermitID         string `json:"permit_id"`
	PermitJWS        string `json:"permit_jws"`
	PermitDigest     string `json:"permit_digest"`
	ActionCanonical  string `json:"action_canonical"`
	ActionDigest     string `json:"action_digest"`
	AmountMinor      int64  `json:"amount_minor"`
	Currency         string `json:"currency"`
	SupplierRef      string `json:"supplier_ref"`
	SupplierRevision int64  `json:"supplier_revision"`
	DeliveryRef      string `json:"delivery_ref"`
	// The instruction's trusted source key, issuer and creation digest
	// (R1-DEF-05). With them, recovery can rebuild an instruction that an
	// engine restore lost. Empty in intents written before they were added.
	SourceSystem    string `json:"source_system,omitempty"`
	SourceRecordID  string `json:"source_record_id,omitempty"`
	IssuerPrincipal string `json:"issuer_principal,omitempty"`
	CreationDigest  string `json:"creation_request_digest,omitempty"`
}

// OutcomeRecord is the OUTCOME payload: the destination result and the
// resulting engine status and budget move.
type OutcomeRecord struct {
	Tenant           string `json:"tenant_id"`
	OpRef            string `json:"op_ref"`
	Destination      string `json:"destination"`
	Outcome          string `json:"outcome"`
	Status           string `json:"status"`
	Move             string `json:"move"`
	DestinationOrder string `json:"destination_order,omitempty"`
	DestinationSeq   int64  `json:"destination_seq"`
	Incarnation      string `json:"incarnation,omitempty"`
	BlockCommitted   *bool  `json:"block_committed"`
	PriorCreate      *bool  `json:"prior_create"`
	CreateDone       bool   `json:"create_done,omitempty"`
	RepairID         string `json:"repair_id,omitempty"`
}

// Querier lets an append run on the pool or inside an anchor transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Append adds an engine record. requireGeneration > 0 refuses the append
// (ErrWritesDisabled) unless writes are enabled at exactly that generation.
func (c *Client) Append(ctx context.Context, typ, tenant, opRef string, payload any, requireGeneration int64) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var gen *int64
	if requireGeneration > 0 {
		gen = &requireGeneration
	}
	var seq int64
	err = c.Pool.QueryRow(ctx, `SELECT anchor.append_engine($1, $2, $3, $4::jsonb, $5)`, typ, tenant, opRef, b, gen).Scan(&seq)
	return seq, classify(err)
}

// WriteState returns the current write switch.
func (c *Client) WriteState(ctx context.Context) (WriteState, error) {
	var b []byte
	if err := c.Pool.QueryRow(ctx, `SELECT anchor.current_write_state()`).Scan(&b); err != nil {
		return WriteState{}, classify(err)
	}
	var ws WriteState
	return ws, json.Unmarshal(b, &ws)
}

// DisableWrites turns writes off and bumps the generation (a no-op that
// keeps the first reason when writes are already off).
func (c *Client) DisableWrites(ctx context.Context, reason string) (WriteState, error) {
	return c.setWriteState(ctx, false, reason, nil, 0)
}

// EnableWrites turns writes on at generation gen, recording the destination
// baseline sequence. It is refused if the generation changed.
func (c *Client) EnableWrites(ctx context.Context, gen int64, reason string, baselineSeq int64) (WriteState, error) {
	return c.setWriteState(ctx, true, reason, &gen, baselineSeq)
}

func (c *Client) setWriteState(ctx context.Context, enabled bool, reason string, gen *int64, baseline int64) (WriteState, error) {
	var b []byte
	if err := c.Pool.QueryRow(ctx, `SELECT anchor.set_write_state($1, $2, $3, $4)`, enabled, reason, gen, baseline).Scan(&b); err != nil {
		return WriteState{}, classify(err)
	}
	var ws WriteState
	return ws, json.Unmarshal(b, &ws)
}

// Records returns every engine record of one type, in sequence order.
func (c *Client) Records(ctx context.Context, typ string) ([]Record, error) {
	return c.query(ctx, `SELECT seq, record_type, tenant_id, op_ref, payload, recorded_at FROM anchor.engine_records WHERE record_type = $1 ORDER BY seq`, typ)
}

// RecordsFor returns every engine record of one operation, in sequence order.
func (c *Client) RecordsFor(ctx context.Context, tenant, opRef string) ([]Record, error) {
	return c.query(ctx, `SELECT seq, record_type, tenant_id, op_ref, payload, recorded_at FROM anchor.engine_records
		WHERE tenant_id = $1 AND op_ref = $2 ORDER BY seq`, tenant, opRef)
}

// RecordsForMandate reads the recovery history of one budget. The caller
// holds that mandate's engine row lock, also used by dispatch and settlement,
// so an anchor append cannot be mistaken for an uncommitted engine write.
func (c *Client) RecordsForMandate(ctx context.Context, tenant, mandate string) ([]Record, error) {
	return c.query(ctx, `SELECT r.seq, r.record_type, r.tenant_id, r.op_ref, r.payload, r.recorded_at
		FROM anchor.engine_records r WHERE r.tenant_id = $1 AND r.op_ref IN
		(SELECT op_ref FROM anchor.engine_records WHERE tenant_id = $1
		 AND record_type = 'DISPATCH_INTENT' AND payload->>'mandate_id' = $2)
		ORDER BY r.seq`, tenant, mandate)
}

func (c *Client) query(ctx context.Context, sql string, args ...any) ([]Record, error) {
	rows, err := c.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Seq, &r.Type, &r.Tenant, &r.OpRef, &r.Payload, &r.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, classify(rows.Err())
}

// Now is the anchor's clock.
func (c *Client) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	return t, classify(c.Pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t))
}

// Intents returns every DISPATCH_INTENT, decoded.
func (c *Client) Intents(ctx context.Context) ([]IntentRecord, error) {
	recs, err := c.Records(ctx, DispatchIntent)
	if err != nil {
		return nil, err
	}
	out := make([]IntentRecord, 0, len(recs))
	for _, r := range recs {
		var ir IntentRecord
		if err := json.Unmarshal(r.Payload, &ir); err != nil {
			return nil, fmt.Errorf("anchor record %d: %w", r.Seq, err)
		}
		out = append(out, ir)
	}
	return out, nil
}

// LatestOutcomes returns the latest OUTCOME per (tenant, op_ref).
func (c *Client) LatestOutcomes(ctx context.Context) (map[[2]string]OutcomeRecord, error) {
	recs, err := c.Records(ctx, Outcome)
	if err != nil {
		return nil, err
	}
	out := map[[2]string]OutcomeRecord{}
	for _, r := range recs {
		var o OutcomeRecord
		if err := json.Unmarshal(r.Payload, &o); err != nil {
			return nil, fmt.Errorf("anchor record %d: %w", r.Seq, err)
		}
		out[[2]string{r.Tenant, r.OpRef}] = o
	}
	return out, nil
}

// ExpectedMinSeq is the highest destination sequence the anchor has seen
// since writes were last enabled, or that enable's baseline if higher. The
// engine sends it with every create and fence, so the first write after a
// destination restore is refused.
func (c *Client) ExpectedMinSeq(ctx context.Context, destination string) (int64, error) {
	return c.ExpectedMinSeqBefore(ctx, destination, time.Time{})
}

// ExpectedMinSeqBefore is ExpectedMinSeq counting only outcomes recorded at
// or before cutoff (zero: all), so a restore check never compares the
// destination with a result recorded after it read the destination.
func (c *Client) ExpectedMinSeqBefore(ctx context.Context, destination string, cutoff time.Time) (int64, error) {
	var lim *time.Time
	if !cutoff.IsZero() {
		lim = &cutoff
	}
	var n int64
	err := c.Pool.QueryRow(ctx, `WITH w AS (
			SELECT COALESCE(max(seq), 0) AS seq FROM anchor.engine_records WHERE record_type = 'WRITE_STATE' AND (payload->>'enabled')::boolean),
		b AS (SELECT COALESCE((SELECT (payload->>'baseline_seq')::bigint FROM anchor.engine_records, w
			WHERE record_type = 'WRITE_STATE' AND engine_records.seq = w.seq), 0) AS baseline)
		SELECT GREATEST((SELECT baseline FROM b), COALESCE((SELECT max((payload->>'destination_seq')::bigint) FROM anchor.engine_records, w
			WHERE record_type = 'OUTCOME' AND engine_records.seq > w.seq AND payload->>'destination' = $1
			AND ($2::timestamptz IS NULL OR recorded_at <= $2)), 0))`, destination, lim).Scan(&n)
	return n, classify(err)
}

// DestinationRecords returns every destination record for destination.
func (c *Client) DestinationRecords(ctx context.Context, destination string) ([]DestRecord, error) {
	return destRecords(ctx, c.Pool, destination)
}

func destRecords(ctx context.Context, p *pgxpool.Pool, destination string) ([]DestRecord, error) {
	rows, err := p.Query(ctx, `SELECT seq, record_type, destination, op_ref, payload FROM anchor.destination_records WHERE destination = $1 ORDER BY seq`, destination)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var out []DestRecord
	for rows.Next() {
		var r DestRecord
		if err := rows.Scan(&r.Seq, &r.Type, &r.Destination, &r.OpRef, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, classify(rows.Err())
}

// ProofFor folds the destination records of one op_ref, read with the
// engine's own anchor identity (not through the destination connector).
func (c *Client) ProofFor(ctx context.Context, destination, opRef string) (connector.Proof, error) {
	rows, err := c.Pool.Query(ctx, `SELECT seq, record_type, destination, op_ref, payload FROM anchor.destination_records
		WHERE destination = $1 AND op_ref = $2 ORDER BY seq`, destination, opRef)
	if err != nil {
		return connector.Proof{}, classify(err)
	}
	defer rows.Close()
	var recs []DestRecord
	for rows.Next() {
		var r DestRecord
		if err := rows.Scan(&r.Seq, &r.Type, &r.Destination, &r.OpRef, &r.Payload); err != nil {
			return connector.Proof{}, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return connector.Proof{}, classify(err)
	}
	m, err := foldProofs(recs)
	if err != nil {
		return connector.Proof{}, err
	}
	return m[opRef], nil
}

// Proofs folds the destination records into one Proof per op_ref.
func (c *Client) Proofs(ctx context.Context, destination string) (map[string]connector.Proof, error) {
	recs, err := c.DestinationRecords(ctx, destination)
	if err != nil {
		return nil, err
	}
	return foldProofs(recs)
}

func foldProofs(recs []DestRecord) (map[string]connector.Proof, error) {
	out := map[string]connector.Proof{}
	for _, r := range recs {
		p := out[r.OpRef]
		switch r.Type {
		case CreateIntent:
			p.CreateIntent = true
		case Block:
			p.Blocked = true
		case CreateDone:
			var o connector.Order
			if err := json.Unmarshal(r.Payload, &o); err != nil {
				return nil, fmt.Errorf("destination record %d: %w", r.Seq, err)
			}
			p.CreateDone = &o
		}
		out[r.OpRef] = p
	}
	return out, nil
}

// Verify recomputes both hash chains and checks that sequences are
// contiguous from 1 and end at the stream heads. Any edit, deletion or
// insertion outside the append functions is reported.
func Verify(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) error {
	for _, s := range []struct{ stream, table, scope string }{
		{"engine", "anchor.engine_records", "tenant_id"},
		{"destination", "anchor.destination_records", "destination"},
	} {
		rows, err := q.Query(ctx, `SELECT seq, record_type, `+s.scope+`, op_ref, payload::text, prev_hash, hash FROM `+s.table+` ORDER BY seq`)
		if err != nil {
			return classify(err)
		}
		prev := make([]byte, 32)
		var want int64 = 1
		for rows.Next() {
			var seq int64
			var typ, scope, op, payload string
			var ph, h []byte
			if err := rows.Scan(&seq, &typ, &scope, &op, &payload, &ph, &h); err != nil {
				rows.Close()
				return err
			}
			if seq != want {
				rows.Close()
				return fmt.Errorf("anchor %s stream: expected record %d, found %d", s.stream, want, seq)
			}
			if !bytes.Equal(ph, prev) {
				rows.Close()
				return fmt.Errorf("anchor %s stream: record %d does not chain to the previous record", s.stream, seq)
			}
			sum := sha256.Sum256(append(append([]byte{}, prev...), []byte(typ+"|"+scope+"|"+op+"|"+payload)...))
			if !bytes.Equal(sum[:], h) {
				rows.Close()
				return fmt.Errorf("anchor %s stream: record %d was changed", s.stream, seq)
			}
			prev, want = h, want+1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return classify(err)
		}
		var last int64
		var lastHash []byte
		if err := q.QueryRow(ctx, `SELECT last_seq, last_hash FROM anchor.stream_heads WHERE stream = $1`, s.stream).Scan(&last, &lastHash); err != nil {
			return classify(err)
		}
		if last != want-1 || !bytes.Equal(lastHash, prev) {
			return fmt.Errorf("anchor %s stream: head is at %d but the records end at %d", s.stream, last, want-1)
		}
	}
	return nil
}

// DestStream is the destination module's restore-proof record
// (connector.RestoreProof), using the anchor_dest_writer identity: it can
// append and read destination records only.
type DestStream struct {
	Pool        *pgxpool.Pool
	Destination string
}

var _ connector.RestoreProof = (*DestStream)(nil)

// Proof implements connector.RestoreProof.
func (d *DestStream) Proof(ctx context.Context, opRef string) (connector.Proof, error) {
	rows, err := d.Pool.Query(ctx, `SELECT seq, record_type, destination, op_ref, payload FROM anchor.destination_records
		WHERE destination = $1 AND op_ref = $2 ORDER BY seq`, d.Destination, opRef)
	if err != nil {
		return connector.Proof{}, classify(err)
	}
	defer rows.Close()
	var recs []DestRecord
	for rows.Next() {
		var r DestRecord
		if err := rows.Scan(&r.Seq, &r.Type, &r.Destination, &r.OpRef, &r.Payload); err != nil {
			return connector.Proof{}, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return connector.Proof{}, classify(err)
	}
	m, err := foldProofs(recs)
	if err != nil {
		return connector.Proof{}, err
	}
	return m[opRef], nil
}

func (d *DestStream) append(ctx context.Context, typ, opRef string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var seq int64
	return classify(d.Pool.QueryRow(ctx, `SELECT anchor.append_destination($1, $2, $3, $4::jsonb)`, typ, d.Destination, opRef, b).Scan(&seq))
}

// RecordCreateIntent implements connector.RestoreProof.
func (d *DestStream) RecordCreateIntent(ctx context.Context, opRef, permitDigest string) error {
	return d.append(ctx, CreateIntent, opRef, map[string]string{"op_ref": opRef, "permit_digest": permitDigest})
}

// RecordBlock implements connector.RestoreProof.
func (d *DestStream) RecordBlock(ctx context.Context, opRef, reason string) error {
	return d.append(ctx, Block, opRef, map[string]string{"op_ref": opRef, "reason": reason})
}

// RecordCreateDone implements connector.RestoreProof.
func (d *DestStream) RecordCreateDone(ctx context.Context, opRef string, o connector.Order) error {
	return d.append(ctx, CreateDone, opRef, o)
}

func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "SM003" {
		return fmt.Errorf("%w: %s", ErrWritesDisabled, pgErr.Message)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "SM005" {
		return fmt.Errorf("%w: %s", ErrBlocked, pgErr.Message)
	}
	return err
}
