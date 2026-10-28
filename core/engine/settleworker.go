package engine

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errNotSettled marks a settle round trip that left the operation PENDING or
// UNRESOLVED (destination unavailable, outcome unknown, block not yet
// committed). The operation is retried with backoff; this is not a fault.
var errNotSettled = errors.New("not settled yet")

// SettleDue advances every dispatched R1 operation that is still PENDING or
// UNRESOLVED and not backed off, one connector round trip each, through
// e.Connector (lookup-first recovery, D2). An operation that stays
// unsettled, or whose round trip fails, is backed off on its own with the
// same capped exponential schedule as the R0 workers, so one destination
// problem cannot hold back other operations. HELD and MISMATCH operations
// are never touched: they wait for an operator finding.
//
// It returns how many operations reached a final state (COMMITTED, FENCED,
// MISMATCH or HELD). The error reports only connector or database faults,
// not operations that simply remain unresolved.
func (e *Engine) SettleDue(ctx context.Context) (int, error) {
	if e.Connector == nil {
		return 0, fault("SERVICE_UNAVAILABLE", "no destination connector is configured")
	}
	rows, err := e.Pool.Query(ctx, `SELECT d.tenant_id, d.op_ref FROM steelmatter.dispatches d
		WHERE d.status IN ('PENDING', 'UNRESOLVED') AND `+notBackedOff(workerSettle, "d.tenant_id", "d.op_ref")+`
		ORDER BY d.updated_at, d.tenant_id, d.op_ref LIMIT 100`)
	if err != nil {
		return 0, unavailable(err)
	}
	type cand struct{ tenant, opRef string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.tenant, &c.opRef); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, classify(err)
	}
	settled, failed := 0, 0
	var first error
	var okTenants, okRecords []string
	for _, c := range cands {
		err := e.settleOne(ctx, c.tenant, c.opRef)
		if ctx.Err() != nil {
			return settled, ctx.Err()
		}
		switch {
		case err == nil:
			settled++
			okTenants, okRecords = append(okTenants, c.tenant), append(okRecords, c.opRef)
		case errors.Is(err, errNotSettled):
			if rerr := e.recordFailure(ctx, workerSettle, c.tenant, c.opRef, err); rerr != nil {
				return settled, rerr
			}
		default:
			failed++
			if first == nil {
				first = fmt.Errorf("%s/%s: %w", c.tenant, c.opRef, err)
			}
			if rerr := e.recordFailure(ctx, workerSettle, c.tenant, c.opRef, err); rerr != nil {
				return settled, fmt.Errorf("%w (and recording the failure: %v)", first, rerr)
			}
		}
	}
	if err := e.resolveBackoff(ctx, workerSettle, okTenants, okRecords); err != nil {
		return settled, err
	}
	if failed > 0 {
		return settled, fmt.Errorf("%d of %d dispatched operations failed to settle and were backed off; first %w", failed, len(cands), first)
	}
	return settled, nil
}

func (e *Engine) settleOne(ctx context.Context, tenant, opRef string) error {
	r, err := e.Settle(ctx, tenant, opRef, e.Connector)
	if err != nil {
		return err
	}
	if r.Execution == DispatchPending || r.Execution == DispatchUnresolved {
		return fmt.Errorf("%w: %s after %s", errNotSettled, r.Execution, r.LastOutcome)
	}
	return nil
}

// inFlightSettleTimeout bounds each immediate settle attempt after a
// revocation; the worker keeps retrying anything left unsettled.
const inFlightSettleTimeout = 5 * time.Second

// settleNow makes one immediate settle attempt for each operation (D4: after
// a revocation, in-flight operations go straight to lookup and fence).
// Failures are left to the settle worker, whose backoff for these records was
// cleared when they were listed.
func (e *Engine) settleNow(ctx context.Context, tenant string, opRefs []string) {
	if e.Connector == nil {
		return
	}
	for _, op := range opRefs {
		c, cancel := context.WithTimeout(ctx, inFlightSettleTimeout)
		_, _ = e.Settle(c, tenant, op, e.Connector)
		cancel()
	}
}
