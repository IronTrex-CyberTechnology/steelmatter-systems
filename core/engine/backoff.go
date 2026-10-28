package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Per-record failure isolation for the background workers (SEC-DEF-002,
// SEC-DEF-003, SEC-DEF-005). A record that fails is stored in
// steelmatter.worker_backoff with capped exponential backoff and skipped
// until its next attempt, so it cannot hold back later records in any tenant
// or fill a worker's batch window. When it later succeeds its row is marked
// resolved (the executor cannot delete, T22). Conditions that affect every record (a
// retired signing key, the database being down) are refused before the loop
// and are never recorded against individual records.

const (
	workerCleanup  = "cleanup"
	workerEvidence = "evidence"
	workerObserver = "observer"
	workerSettle   = "settle"
	workerOutbox   = "outbox"

	backoffBase = 10 * time.Second // first retry delay; doubles per attempt
	backoffMax  = time.Hour        // retries continue at most this far apart

	// BacklogAlertAge is how long a record may stay owed by a worker before
	// RunWorkers reports it.
	BacklogAlertAge = 5 * time.Minute

	maxErrorText = 1000
)

// notBackedOff is a SQL predicate excluding records whose next attempt is
// still in the future. Arguments are constants and column names only.
func notBackedOff(worker, tenantCol, recordCol string) string {
	return `NOT EXISTS (SELECT 1 FROM steelmatter.worker_backoff w WHERE w.worker = '` + worker + `'
		AND w.tenant_id = ` + tenantCol + ` AND w.record_id = ` + recordCol + ` AND w.resolved_at IS NULL AND w.next_attempt_at > clock_timestamp())`
}

// runIsolated applies fn to every item. A failing item is recorded for
// backoff and the remaining items still run. If any failed, the error names
// how many and the first failure. Records that succeed have any backoff
// marked resolved. Cancellation stops the loop without recording anything.
func runIsolated[T any](ctx context.Context, e *Engine, worker string, items []T, key func(T) (tenant, record string), fn func(T) error) error {
	var okTenants, okRecords []string
	failed := 0
	var first error
	for _, it := range items {
		tenant, record := key(it)
		err := fn(it)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			failed++
			if first == nil {
				first = fmt.Errorf("%s/%s: %w", tenant, record, err)
			}
			if rerr := e.recordFailure(ctx, worker, tenant, record, err); rerr != nil {
				return fmt.Errorf("%w (and recording the failure: %v)", first, rerr)
			}
			continue
		}
		okTenants, okRecords = append(okTenants, tenant), append(okRecords, record)
	}
	if err := e.resolveBackoff(ctx, worker, okTenants, okRecords); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d records failed and were backed off; first %w", failed, len(items), first)
	}
	return nil
}

// resolveBackoff marks any open backoff of these records resolved.
func (e *Engine) resolveBackoff(ctx context.Context, worker string, tenants, records []string) error {
	if len(records) == 0 {
		return nil
	}
	if _, err := e.Pool.Exec(ctx, `UPDATE steelmatter.worker_backoff w SET resolved_at = clock_timestamp()
		FROM unnest($2::text[], $3::text[]) AS d(tenant_id, record_id)
		WHERE w.worker = $1 AND w.tenant_id = d.tenant_id AND w.record_id = d.record_id AND w.resolved_at IS NULL`, worker, tenants, records); err != nil {
		return unavailable(err)
	}
	return nil
}

// recordFailure stores cause against the record and schedules its next
// attempt backoffBase·2^(attempts-1) later, capped at backoffMax. A record
// that fails again after being resolved starts over at one attempt.
func (e *Engine) recordFailure(ctx context.Context, worker, tenant, record string, cause error) error {
	msg := cause.Error()
	if len(msg) > maxErrorText {
		msg = strings.ToValidUTF8(msg[:maxErrorText], "")
	}
	_, err := e.Pool.Exec(ctx, `INSERT INTO steelmatter.worker_backoff AS b (worker, tenant_id, record_id, attempts, last_error,
			first_failed_at, last_failed_at, next_attempt_at)
		VALUES ($1, $2, $3, 1, $4, clock_timestamp(), clock_timestamp(), clock_timestamp() + make_interval(secs => $5))
		ON CONFLICT (worker, tenant_id, record_id) DO UPDATE SET last_error = EXCLUDED.last_error, last_failed_at = EXCLUDED.last_failed_at,
			attempts = CASE WHEN b.resolved_at IS NULL THEN b.attempts + 1 ELSE 1 END,
			first_failed_at = CASE WHEN b.resolved_at IS NULL THEN b.first_failed_at ELSE EXCLUDED.first_failed_at END,
			next_attempt_at = CASE WHEN b.resolved_at IS NULL
				THEN EXCLUDED.last_failed_at + make_interval(secs => least($5 * power(2, least(b.attempts, 20)), $6))
				ELSE EXCLUDED.next_attempt_at END,
			resolved_at = NULL`,
		worker, tenant, record, msg, backoffBase.Seconds(), backoffMax.Seconds())
	return err
}

// Backlog is how long the oldest record each worker still owes has been
// waiting: an unsigned execution record or an unobserved operation since
// commit, an unreleased reservation since expiry. Zero means none. BackedOff
// counts records currently waiting for a retry.
//
// R1: Settle is the oldest dispatched operation still PENDING or UNRESOLVED,
// since dispatch; Outbox is the oldest undelivered event that has already
// failed delivery at least once (D6), since it was written; Held counts
// operations HELD or MISMATCH, which wait for an operator finding.
type Backlog struct {
	Evidence, Observer, Cleanup time.Duration
	Settle, Outbox              time.Duration
	BackedOff                   int
	Held                        int
}

// WorkerBacklog reports the current Backlog.
func (e *Engine) WorkerBacklog(ctx context.Context) (Backlog, error) {
	var ev, ob, cl, se, ou float64
	var b Backlog
	err := e.Pool.QueryRow(ctx, `SELECT
		COALESCE(extract(epoch FROM clock_timestamp() - (SELECT min(committed_at) FROM steelmatter.evidence_operations WHERE execution_jws IS NULL)), 0),
		COALESCE(extract(epoch FROM clock_timestamp() - (SELECT min(o.committed_at) FROM steelmatter.evidence_operations o
			WHERE NOT EXISTS (SELECT 1 FROM steelmatter.evidence_observations b WHERE b.tenant_id = o.tenant_id AND b.operation_id = o.operation_id))), 0),
		COALESCE(extract(epoch FROM clock_timestamp() - (SELECT min(expires_at) FROM steelmatter.issuances
			WHERE status IN ('UNSIGNED', 'ACTIVE') AND expires_at <= clock_timestamp())), 0),
		(SELECT count(*) FROM steelmatter.worker_backoff WHERE resolved_at IS NULL AND next_attempt_at > clock_timestamp()),
		COALESCE(extract(epoch FROM clock_timestamp() - (SELECT min(created_at) FROM steelmatter.dispatches WHERE status IN ('PENDING', 'UNRESOLVED'))), 0),
		COALESCE(extract(epoch FROM clock_timestamp() - (SELECT min(o.created_at) FROM steelmatter.outbox o
			WHERE o.delivered_at IS NULL AND EXISTS (SELECT 1 FROM steelmatter.worker_backoff w WHERE w.worker = 'outbox'
				AND w.tenant_id = o.tenant_id AND w.record_id = o.event_id AND w.resolved_at IS NULL))), 0),
		(SELECT count(*) FROM steelmatter.dispatches WHERE status IN ('HELD', 'MISMATCH'))`).Scan(&ev, &ob, &cl, &b.BackedOff, &se, &ou, &b.Held)
	if err != nil {
		return Backlog{}, unavailable(err)
	}
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	b.Evidence, b.Cleanup, b.Settle, b.Outbox = sec(ev), sec(cl), sec(se), sec(ou)
	if e.Observer != nil {
		b.Observer = sec(ob)
	}
	return b, nil
}

// checkBacklog returns an error describing every worker whose oldest owed
// record has waited longer than BacklogAlertAge.
func (e *Engine) checkBacklog(ctx context.Context) error {
	b, err := e.WorkerBacklog(ctx)
	if err != nil {
		return err
	}
	var late []string
	for _, w := range []struct {
		what string
		age  time.Duration
	}{
		{"oldest unsigned execution record", b.Evidence},
		{"oldest unobserved operation", b.Observer},
		{"oldest unreleased expired reservation", b.Cleanup},
		{"oldest unsettled dispatched operation", b.Settle},
		{"oldest failing outbox event", b.Outbox},
	} {
		if w.age > BacklogAlertAge {
			late = append(late, fmt.Sprintf("%s waiting %s", w.what, w.age.Truncate(time.Second)))
		}
	}
	if b.Held > 0 {
		late = append(late, fmt.Sprintf("%d dispatched operations HELD or MISMATCH need an operator finding", b.Held))
	}
	if len(late) == 0 {
		return nil
	}
	return fmt.Errorf("worker backlog: %s (%d records backed off)", strings.Join(late, "; "), b.BackedOff)
}
