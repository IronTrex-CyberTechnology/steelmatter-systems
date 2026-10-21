package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/auth"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/failpoint"
)

// CleanupExpired releases unused (UNSIGNED or ACTIVE) issuances whose expiry
// has passed. Candidates are found without locks; each is then locked in the
// global order and rechecked, so cleanup and admission have one serial
// outcome and committed capacity is never released (T28). Returns the number
// released.
func (e *Engine) CleanupExpired(ctx context.Context) (int, error) {
	rows, err := e.Pool.Query(ctx, `SELECT tenant_id, mandate_id, instruction_id, permit_id FROM steelmatter.issuances
		WHERE status IN ('UNSIGNED', 'ACTIVE') AND expires_at <= clock_timestamp() ORDER BY tenant_id, permit_id LIMIT 100`)
	if err != nil {
		return 0, unavailable(err)
	}
	type cand struct{ tenant, mandate, instruction, permit string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.tenant, &c.mandate, &c.instruction, &c.permit); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, classify(err)
	}
	n := 0
	for _, c := range cands {
		released, err := e.cleanupOne(ctx, c.tenant, c.mandate, c.instruction, c.permit)
		if err != nil {
			return n, err
		}
		if released {
			n++
		}
	}
	return n, nil
}

func (e *Engine) cleanupOne(ctx context.Context, tenant, mandateID, instructionID, permitID string) (bool, error) {
	released := false
	err := e.inTx(ctx, "", func(tx pgx.Tx) error {
		released = false
		if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, tenant, mandateID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`, tenant, instructionID); err != nil {
			return err
		}
		var rev int64
		if err := tx.QueryRow(ctx, `SELECT action_revision FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, tenant, permitID).Scan(&rev); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
			tenant, instructionID, rev); err != nil {
			return err
		}
		is, err := scanIssuance(tx.QueryRow(ctx, `SELECT `+issuanceCols+` FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`, tenant, permitID))
		if err != nil {
			return err
		}
		if err := failpoint.Inject(ctx, failpoint.CleanupAfterLocks); err != nil {
			return err
		}
		if (is.Status != "UNSIGNED" && is.Status != "ACTIVE") || !is.Expired {
			return nil // consumed, released or not yet expired: nothing to do
		}
		reason := "EXPIRED"
		if is.Status == "UNSIGNED" {
			reason = "RECOVERED_UNUSED" // signing never completed; provably unused
		}
		if err := releaseIssuance(ctx, tx, tenant, mandateID, is, reason); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

// Event is one outbox record.
type Event struct {
	EventID     string
	Tenant      string
	Type        string
	AggregateID string
	Payload     json.RawMessage
}

// Sink receives outbox events. Delivery is at-least-once; sinks deduplicate
// by EventID.
type Sink interface {
	Deliver(ctx context.Context, ev Event) error
}

// ReceiptSink is the synthetic R0 consumer: it records each event once by
// event_id and counts repeated deliveries.
type ReceiptSink struct{ E *Engine }

// Deliver records ev in steelmatter.event_receipts.
func (s ReceiptSink) Deliver(ctx context.Context, ev Event) error {
	_, err := s.E.Pool.Exec(ctx, `INSERT INTO steelmatter.event_receipts (event_id, tenant_id, event_type, aggregate_id, first_received_at, deliveries)
		VALUES ($1, $2, $3, $4, clock_timestamp(), 1)
		ON CONFLICT (event_id) DO UPDATE SET deliveries = steelmatter.event_receipts.deliveries + 1`,
		ev.EventID, ev.Tenant, ev.Type, ev.AggregateID)
	return err
}

// DispatchOutbox delivers undelivered events with their original event IDs,
// then marks them delivered. A crash between delivery and marking causes
// redelivery of the same event ID (T15). Returns the number delivered.
func (e *Engine) DispatchOutbox(ctx context.Context, sink Sink) (int, error) {
	rows, err := e.Pool.Query(ctx, `SELECT event_id, tenant_id, event_type, aggregate_id, payload FROM steelmatter.outbox
		WHERE delivered_at IS NULL ORDER BY created_at, event_id LIMIT 100`)
	if err != nil {
		return 0, unavailable(err)
	}
	var evs []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.EventID, &ev.Tenant, &ev.Type, &ev.AggregateID, &ev.Payload); err != nil {
			rows.Close()
			return 0, err
		}
		evs = append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, classify(err)
	}
	n := 0
	for _, ev := range evs {
		if err := sink.Deliver(ctx, ev); err != nil {
			return n, err
		}
		if err := failpoint.Inject(ctx, failpoint.OutboxAfterDeliver); err != nil {
			return n, err
		}
		if _, err := e.Pool.Exec(ctx, `UPDATE steelmatter.outbox SET delivered_at = clock_timestamp() WHERE event_id = $1 AND delivered_at IS NULL`, ev.EventID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RunWorkers runs cleanup, unsigned-issuance recovery and outbox dispatch
// every interval until ctx ends. Errors are reported to onErr and retried.
func (e *Engine) RunWorkers(ctx context.Context, interval time.Duration, sink Sink, onErr func(string, error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := e.RecoverUnsigned(ctx); err != nil {
			onErr("recovery", err)
		}
		if _, err := e.CleanupExpired(ctx); err != nil {
			onErr("cleanup", err)
		}
		if _, err := e.DispatchOutbox(ctx, sink); err != nil {
			onErr("outbox", err)
		}
		if _, err := e.SignPendingExecutions(ctx); err != nil {
			onErr("evidence", err)
		}
		if _, err := e.ObserveUnobserved(ctx); err != nil {
			onErr("observer", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// OperationStatus reports the four independent status fields.
type OperationStatus struct {
	OperationID   string     `json:"operation_id"`
	InstructionID string     `json:"instruction_id"`
	Authorisation string     `json:"authorisation"`
	Execution     string     `json:"execution"`
	Observation   string     `json:"observation"`
	Evidence      string     `json:"evidence"`
	TargetRef     *TargetRef `json:"target_ref,omitempty"`
}

// GetOperation returns status for an authorised reader in the same tenant:
// the instruction's actor or issuer, the mandate owner, an observer or an
// operator. Anyone else gets NOT_FOUND without disclosure (T23).
func (e *Engine) GetOperation(ctx context.Context, p auth.Principal, operationID string) (*OperationStatus, error) {
	sc, err := e.scope(ctx, p, operationID, readers...)
	if err != nil {
		return nil, err
	}
	s := OperationStatus{OperationID: operationID, InstructionID: sc.instructionID,
		Authorisation: "PERMITTED", Execution: "NOT_ADMITTED", Observation: "NOT_OBSERVED", Evidence: "PENDING"}
	var reason, orderID, obs *string
	var mandateJWS, permitJWS, execJWS, obsJWS *string // read for completeness; status comes from loadEvidence
	var iStatus string
	var expired bool
	err = e.Pool.QueryRow(ctx, `SELECT i.status, i.release_reason, clock_timestamp() >= i.expires_at, o.target_order_id, m.mandate_jws, i.permit_jws,
		o.execution_jws, b.status, b.observation_jws
		FROM steelmatter.issuances i
		JOIN steelmatter.mandates m ON m.tenant_id = i.tenant_id AND m.mandate_id = i.mandate_id
		LEFT JOIN steelmatter.operations o ON o.tenant_id = i.tenant_id AND o.operation_id = i.operation_id
		LEFT JOIN LATERAL (SELECT status, observation_jws FROM steelmatter.observations x
			WHERE x.tenant_id = i.tenant_id AND x.operation_id = i.operation_id ORDER BY recorded_at DESC LIMIT 1) b ON true
		WHERE i.tenant_id = $1 AND i.operation_id = $2`, p.Tenant, operationID).
		Scan(&iStatus, &reason, &expired, &orderID, &mandateJWS, &permitJWS, &execJWS, &obs, &obsJWS)
	if err != nil {
		return nil, unavailable(err)
	}
	// Derivation (F-19, documented in api/openapi.yaml OperationStatus):
	// consumed → PERMITTED; released by revocation or withdrawn by amendment
	// → REVOKED; expired (cleaned up or not) or unused unsigned → EXPIRED.
	// DENIED is unreachable (denials create no operation).
	switch {
	case iStatus == "CONSUMED":
		s.Authorisation = "PERMITTED"
	case reason != nil && (*reason == "REVOKED" || *reason == "AMENDED"):
		s.Authorisation = "REVOKED"
	case reason != nil || expired:
		s.Authorisation = "EXPIRED"
	}
	if orderID != nil {
		s.Execution = "COMMITTED"
		s.TargetRef = &TargetRef{AdapterID: e.Cfg.AdapterID, OrderID: *orderID}
	}
	if obs != nil {
		s.Observation = *obs
	}
	ev, err := e.loadEvidence(ctx, p.Tenant, operationID, sc.instructionID)
	if err != nil {
		return nil, err
	}
	s.Evidence = ev.Evidence
	return &s, nil
}
