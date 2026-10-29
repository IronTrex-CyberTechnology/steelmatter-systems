package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// CleanupExpired releases unused (UNSIGNED or ACTIVE) issuances whose expiry
// has passed. Candidates are found without locks; each is then locked in the
// global order and rechecked, so cleanup and admission have one serial
// outcome and committed capacity is never released (T28). A candidate whose
// release fails is backed off and the rest are still released (SEC-DEF-005).
// Returns the number released.
func (e *Engine) CleanupExpired(ctx context.Context) (int, error) {
	rows, err := e.Pool.Query(ctx, `SELECT i.tenant_id, i.mandate_id, i.instruction_id, i.permit_id FROM steelmatter.issuances i
		WHERE i.status IN ('UNSIGNED', 'ACTIVE') AND i.expires_at <= clock_timestamp()
		AND `+notBackedOff(workerCleanup, "i.tenant_id", "i.permit_id")+`
		ORDER BY i.tenant_id, i.permit_id LIMIT 100`)
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
	err = runIsolated(ctx, e, workerCleanup, cands, func(c cand) (string, string) { return c.tenant, c.permit }, func(c cand) error {
		released, err := e.cleanupOne(failpoint.WithKey(ctx, c.permit), c.tenant, c.mandate, c.instruction, c.permit)
		if released {
			n++
		}
		return err
	})
	return n, err
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
	// AggregateSeq orders events within one aggregate (D6).
	AggregateSeq int64
	Payload      json.RawMessage
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
//
// Ordering is per aggregate (D6): only the oldest undelivered event of each
// aggregate (one operation or one mandate) is eligible, so events of one
// aggregate arrive in aggregate_seq order. A failing event is backed off on
// its own and holds back only the later events of its aggregate; every other
// aggregate keeps flowing. No global order is promised.
func (e *Engine) DispatchOutbox(ctx context.Context, sink Sink) (int, error) {
	n := 0
	var firstErr error
	for round := 0; round < outboxRounds; round++ {
		evs, err := e.outboxHeads(ctx)
		if err != nil {
			return n, err
		}
		if len(evs) == 0 {
			break
		}
		delivered := 0
		err = runIsolated(ctx, e, workerOutbox, evs, func(ev Event) (string, string) { return ev.Tenant, ev.EventID }, func(ev Event) error {
			if err := sink.Deliver(ctx, ev); err != nil {
				return err
			}
			if err := failpoint.Inject(ctx, failpoint.OutboxAfterDeliver); err != nil {
				return err
			}
			if _, err := e.Pool.Exec(ctx, `UPDATE steelmatter.outbox SET delivered_at = clock_timestamp() WHERE event_id = $1 AND delivered_at IS NULL`, ev.EventID); err != nil {
				return err
			}
			delivered++
			return nil
		})
		n += delivered
		if err != nil {
			if ctx.Err() != nil {
				return n, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if delivered == 0 {
			break
		}
	}
	return n, firstErr
}

// outboxRounds bounds how far one DispatchOutbox call walks down each
// aggregate's queue.
const outboxRounds = 10

// outboxHeads returns the oldest undelivered event of each aggregate whose
// delivery is not currently backed off.
func (e *Engine) outboxHeads(ctx context.Context) ([]Event, error) {
	rows, err := e.Pool.Query(ctx, `SELECT o.event_id, o.tenant_id, o.event_type, o.aggregate_id, o.aggregate_seq, o.payload FROM steelmatter.outbox o
		WHERE o.delivered_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM steelmatter.outbox p WHERE p.tenant_id = o.tenant_id AND p.aggregate_id = o.aggregate_id
			AND p.delivered_at IS NULL AND p.aggregate_seq < o.aggregate_seq)
		AND `+notBackedOff(workerOutbox, "o.tenant_id", "o.event_id")+`
		ORDER BY o.created_at, o.event_id LIMIT 100`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var evs []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.EventID, &ev.Tenant, &ev.Type, &ev.AggregateID, &ev.AggregateSeq, &ev.Payload); err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return evs, nil
}

// RunWorkers runs cleanup, unsigned-issuance recovery, R1 settlement (when a
// connector is configured), outbox dispatch,
// evidence signing and observation every interval until ctx ends, then
// reports any worker backlog older than BacklogAlertAge. Errors are reported
// to onErr and retried.
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
		if e.Connector != nil && e.Anchor != nil {
			if r, err := e.CheckRestore(ctx, e.Connector); err != nil {
				onErr("restore-check", err)
			} else if r.WritesDisabled {
				onErr("restore-check", fmt.Errorf("writes disabled: %d restore signals; reconcile and run the resume gate", len(r.Signals)))
			}
		}
		if e.Connector != nil {
			if _, err := e.SettleDue(ctx); err != nil {
				onErr("settle", err)
			}
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
		if _, err := e.ObserveR1Due(ctx); err != nil {
			onErr("observer-refresh", err)
		}
		if err := e.checkBacklog(ctx); err != nil {
			onErr("backlog", err)
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
		LEFT JOIN steelmatter.evidence_operations o ON o.tenant_id = i.tenant_id AND o.operation_id = i.operation_id
		LEFT JOIN LATERAL (SELECT status, observation_jws FROM steelmatter.evidence_observations x
			WHERE x.tenant_id = i.tenant_id AND x.operation_id = i.operation_id ORDER BY recorded_at DESC LIMIT 1) b ON true
		WHERE i.tenant_id = $1 AND i.operation_id = $2`, p.Tenant, operationID).
		Scan(&iStatus, &reason, &expired, &orderID, &mandateJWS, &permitJWS, &execJWS, &obs, &obsJWS)
	if err != nil {
		return nil, unavailable(err)
	}
	// Derivation (F-19, documented in api/openapi.yaml OperationStatus):
	// consumed → PERMITTED; released by revocation or withdrawn by amendment
	// → REVOKED; expired (cleaned up or not), unused unsigned, or replaced
	// after its signing key was retired (KEY_RETIRED, ADR-004) → EXPIRED.
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
	// R1 dispatched operation: execution is the dispatch status (draft 7).
	// Only COMMITTED is success; a destination order is shown only once
	// committed, never for an order that is held or mismatched.
	if d, err := e.GetDispatch(ctx, p.Tenant, operationID); err != nil {
		return nil, err
	} else if d != nil {
		s.Execution = d.Execution
		if d.Execution == DispatchCommitted && d.DestinationOrder != "" {
			s.TargetRef = &TargetRef{AdapterID: e.Cfg.AdapterID, OrderID: d.DestinationOrder}
		}
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
