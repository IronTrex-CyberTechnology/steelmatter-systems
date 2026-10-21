//go:build integration

// Package locking is the I02 locking prototype (checklist item 6). It runs the
// proposed lock order and post-lock rechecks against real PostgreSQL with
// controlled barriers and shows that each race has one serial outcome.
//
// PROTOTYPE, NOT PRODUCT: these operations are minimal SQL stand-ins for the
// I03-I06 engine (no signing, no HTTP). Results here are prototype evidence
// for I02 and do not mark T01, T07, T08, T10 or T28 PASS.
package locking

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Outcomes. Rejections use the stable codes from core/httpapi.
const (
	Committed          = "COMMITTED"
	Reserved           = "RESERVED"
	Revoked            = "REVOKED"
	AlreadyRevoked     = "ALREADY_REVOKED"
	Released           = "RELEASED"
	SkippedNotUnused   = "SKIPPED_NOT_UNUSED"
	SkippedNotExpired  = "SKIPPED_NOT_EXPIRED"
	Amended            = "AMENDED"
	SignaturePersisted = "SIGNATURE_PERSISTED"
	SignatureDiscarded = "SIGNATURE_DISCARDED"

	AuthorityRevoked      = "AUTHORITY_REVOKED"
	MandateExpired        = "MANDATE_EXPIRED"
	RevisionStale         = "REVISION_STALE"
	PermitNotActive       = "PERMIT_NOT_ACTIVE"
	PermitExpired         = "PERMIT_EXPIRED"
	SupplierRevisionStale = "SUPPLIER_REVISION_STALE"
	AlreadyCommitted      = "ALREADY_COMMITTED"
	CapacityExhausted     = "CAPACITY_EXHAUSTED"
)

// Hook runs while the operation holds all of its locks, before rechecks.
type Hook func()

func noHook() {}

// Times records the transaction start (now()) and the post-lock check time
// (clock_timestamp()) so tests can show why now() must not be used.
type Times struct {
	TxStart, CheckedAt string
	Expired            bool // clock_timestamp() >= permit expires_at
	ExpiredByNow       bool // now() >= permit expires_at (the wrong check)
}

// Admit is the admission path: lock mandate → instruction → action →
// issuance → supplier (FOR SHARE), recheck everything after the locks, then
// write order, line, obligation, operation, consumption, accounting and
// outbox in the same transaction.
func Admit(ctx context.Context, tx pgx.Tx, f *Fixture, permitID string, hook Hook, times *Times) (string, error) {
	var mStatus string
	var epoch int64
	var mandateExpired bool
	if err := tx.QueryRow(ctx, `SELECT status, control_epoch, clock_timestamp() >= expires_at
		FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`,
		f.Tenant, f.Mandate).Scan(&mStatus, &epoch, &mandateExpired); err != nil {
		return "", fmt.Errorf("lock mandate: %w", err)
	}
	var currentRev int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions
		WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`, f.Tenant, f.Instruction).Scan(&currentRev); err != nil {
		return "", fmt.Errorf("lock instruction: %w", err)
	}
	var iStatus, operationID string
	var iRev, iEpoch, amount int64
	if err := tx.QueryRow(ctx, `SELECT status, action_revision, control_epoch, amount_minor, operation_id
		FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID).
		Scan(&iStatus, &iRev, &iEpoch, &amount, &operationID); err != nil {
		return "", fmt.Errorf("read issuance: %w", err)
	}
	var aStatus, digest, supplier string
	var supplierRev int64
	if err := tx.QueryRow(ctx, `SELECT status, action_digest, supplier_id, supplier_revision
		FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		f.Tenant, f.Instruction, iRev).Scan(&aStatus, &digest, &supplier, &supplierRev); err != nil {
		return "", fmt.Errorf("lock action: %w", err)
	}
	// Re-read the issuance under its lock (it may have changed while we waited).
	if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
		f.Tenant, permitID).Scan(&iStatus); err != nil {
		return "", fmt.Errorf("lock issuance: %w", err)
	}
	var liveSupplierRev int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2 FOR SHARE`,
		f.Tenant, supplier).Scan(&liveSupplierRev); err != nil {
		return "", fmt.Errorf("lock supplier: %w", err)
	}

	hook()

	// Post-lock rechecks use clock_timestamp(), never now().
	var t Times
	if err := tx.QueryRow(ctx, `SELECT now()::text, clock_timestamp()::text,
		clock_timestamp() >= expires_at, now() >= expires_at
		FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID).
		Scan(&t.TxStart, &t.CheckedAt, &t.Expired, &t.ExpiredByNow); err != nil {
		return "", err
	}
	if times != nil {
		*times = t
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() >= expires_at FROM steelmatter.mandates
		WHERE tenant_id = $1 AND mandate_id = $2`, f.Tenant, f.Mandate).Scan(&mandateExpired); err != nil {
		return "", err
	}
	switch {
	case mStatus != "ACTIVE":
		return AuthorityRevoked, nil
	case mandateExpired:
		return MandateExpired, nil
	case currentRev != iRev || aStatus != "CURRENT":
		return RevisionStale, nil
	case iStatus != "ACTIVE":
		return PermitNotActive, nil
	case iEpoch != epoch:
		return AuthorityRevoked, nil
	case t.Expired:
		return PermitExpired, nil
	case liveSupplierRev != supplierRev:
		return SupplierRevisionStale, nil
	}

	orderID, obligationID := "po-"+f.Instruction, "obl-"+f.Instruction
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO target.purchase_orders (tenant_id, order_id, instruction_id, operation_id, supplier_id, supplier_revision, delivery_id, currency, amount_minor, created_at)
		  VALUES ($1, $2, $3, $4, $5, $6, 'warehouse-a', 'INR', $7, clock_timestamp())`,
			[]any{f.Tenant, orderID, f.Instruction, operationID, supplier, supplierRev, amount}},
		{`INSERT INTO target.purchase_order_lines (tenant_id, order_id, item_id, quantity, unit_price_minor) VALUES ($1, $2, 'item-a', 1, $3)`,
			[]any{f.Tenant, orderID, amount}},
		{`INSERT INTO steelmatter.obligations (tenant_id, obligation_id, instruction_id, operation_id, action_digest, amount_minor, created_at)
		  VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp())`,
			[]any{f.Tenant, obligationID, f.Instruction, operationID, digest, amount}},
		{`INSERT INTO steelmatter.operations (tenant_id, operation_id, instruction_id, action_revision, permit_id, action_digest, status, target_order_id, obligation_id, committed_at)
		  VALUES ($1, $2, $3, $4, $5, $6, 'COMMITTED', $7, $8, clock_timestamp())`,
			[]any{f.Tenant, operationID, f.Instruction, iRev, permitID, digest, orderID, obligationID}},
		{`UPDATE steelmatter.issuances SET status = 'CONSUMED', consumed_at = clock_timestamp() WHERE tenant_id = $1 AND permit_id = $2`,
			[]any{f.Tenant, permitID}},
		{`UPDATE steelmatter.action_revisions SET status = 'COMMITTED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
			[]any{f.Tenant, f.Instruction, iRev}},
		{`UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, committed_minor = committed_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`,
			[]any{f.Tenant, f.Mandate, amount}},
		{`INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
		  VALUES ($1, $2, 'operation.committed', $3, '{}'::jsonb, clock_timestamp())`,
			[]any{"evt-" + operationID, f.Tenant, operationID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return "", err
		}
	}
	return Committed, nil
}

// Revoke locks the mandate, marks it revoked, increments the control epoch
// and releases unused reservations (proposed answer to I00 Q1).
func Revoke(ctx context.Context, tx pgx.Tx, f *Fixture, hook Hook) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`,
		f.Tenant, f.Mandate).Scan(&status); err != nil {
		return "", err
	}
	hook()
	if status == "REVOKED" {
		return AlreadyRevoked, nil
	}
	// Issuance rows after the mandate row, in primary-key order.
	rows, err := tx.Query(ctx, `SELECT permit_id, amount_minor FROM steelmatter.issuances
		WHERE tenant_id = $1 AND mandate_id = $2 AND status IN ('UNSIGNED', 'ACTIVE') ORDER BY permit_id FOR UPDATE`, f.Tenant, f.Mandate)
	if err != nil {
		return "", err
	}
	var released int64
	var ids []string
	for rows.Next() {
		var id string
		var amt int64
		if err := rows.Scan(&id, &amt); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, id)
		released += amt
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'REVOKED', released_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, id); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET status = 'REVOKED', control_epoch = control_epoch + 1, revoked_at = clock_timestamp(),
		reserved_minor = reserved_minor - $3, available_minor = available_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`,
		f.Tenant, f.Mandate, released); err != nil {
		return "", err
	}
	return Revoked, nil
}

// Cleanup releases one unused, expired reservation under the global lock
// order. Candidates are found without locks; this rechecks under locks.
func Cleanup(ctx context.Context, tx pgx.Tx, f *Fixture, permitID string, hook Hook) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, f.Tenant, f.Mandate); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`, f.Tenant, f.Instruction); err != nil {
		return "", err
	}
	var rev int64
	if err := tx.QueryRow(ctx, `SELECT action_revision FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID).Scan(&rev); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		f.Tenant, f.Instruction, rev); err != nil {
		return "", err
	}
	var status string
	var amount int64
	if err := tx.QueryRow(ctx, `SELECT status, amount_minor FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
		f.Tenant, permitID).Scan(&status, &amount); err != nil {
		return "", err
	}
	hook()
	if status != "UNSIGNED" && status != "ACTIVE" {
		return SkippedNotUnused, nil
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() >= expires_at FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`,
		f.Tenant, permitID).Scan(&expired); err != nil {
		return "", err
	}
	if !expired {
		return SkippedNotExpired, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'EXPIRED', released_at = clock_timestamp()
		WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
		WHERE tenant_id = $1 AND mandate_id = $2`, f.Tenant, f.Mandate, amount); err != nil {
		return "", err
	}
	return Released, nil
}

// Amend appends revision expected+1, invalidating the live issuance and
// releasing its reservation once if unused.
func Amend(ctx context.Context, tx pgx.Tx, f *Fixture, amendmentID string, expected int64, hook Hook) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, f.Tenant, f.Mandate); err != nil {
		return "", err
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
		f.Tenant, f.Instruction).Scan(&current); err != nil {
		return "", err
	}
	var aStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		f.Tenant, f.Instruction, current).Scan(&aStatus); err != nil {
		return "", err
	}
	var permitID, iStatus string
	var amount int64
	err := tx.QueryRow(ctx, `SELECT permit_id, status, amount_minor FROM steelmatter.issuances
		WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 AND status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED') FOR UPDATE`,
		f.Tenant, f.Instruction, current).Scan(&permitID, &iStatus, &amount)
	if err != nil && err != pgx.ErrNoRows {
		return "", err
	}
	hook()
	if aStatus == "COMMITTED" || iStatus == "CONSUMED" {
		return AlreadyCommitted, nil
	}
	if current != expected {
		return RevisionStale, nil
	}
	var released int64
	if iStatus == "UNSIGNED" || iStatus == "ACTIVE" {
		released = amount
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'AMENDED', released_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
			WHERE tenant_id = $1 AND mandate_id = $2`, f.Tenant, f.Mandate, released); err != nil {
			return "", err
		}
	}
	next := current + 1
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE steelmatter.action_revisions SET status = 'SUPERSEDED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
			[]any{f.Tenant, f.Instruction, current}},
		{`INSERT INTO steelmatter.action_revisions (tenant_id, instruction_id, action_revision, status, action_canonical, action_digest,
			supplier_id, supplier_revision, delivery_id, audience, amount_minor, created_at)
		  SELECT tenant_id, instruction_id, $3, 'CURRENT', action_canonical, repeat('b', 64), supplier_id, supplier_revision, delivery_id, audience, amount_minor, clock_timestamp()
		  FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $4`,
			[]any{f.Tenant, f.Instruction, next, current}},
		{`UPDATE steelmatter.instructions SET current_revision = $3 WHERE tenant_id = $1 AND instruction_id = $2`,
			[]any{f.Tenant, f.Instruction, next}},
		{`INSERT INTO steelmatter.amendments (tenant_id, instruction_id, issuer_principal, amendment_id, request_digest, expected_revision, result_revision, released_minor, created_at)
		  VALUES ($1, $2, 'issuer-a', $3, repeat('c', 64), $4, $5, $6, clock_timestamp())`,
			[]any{f.Tenant, f.Instruction, amendmentID, expected, next, released}},
	} {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return "", err
		}
	}
	return Amended, nil
}

// PersistSignature publishes a signature computed outside the transaction,
// only if the issuance is still unsigned and its revision is still current.
func PersistSignature(ctx context.Context, tx pgx.Tx, f *Fixture, permitID, jws string, hook Hook) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`, f.Tenant, f.Mandate); err != nil {
		return "", err
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
		f.Tenant, f.Instruction).Scan(&current); err != nil {
		return "", err
	}
	var rev int64
	if err := tx.QueryRow(ctx, `SELECT action_revision FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID).Scan(&rev); err != nil {
		return "", err
	}
	var aStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		f.Tenant, f.Instruction, rev).Scan(&aStatus); err != nil {
		return "", err
	}
	var iStatus string
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT status, clock_timestamp() >= expires_at FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
		f.Tenant, permitID).Scan(&iStatus, &expired); err != nil {
		return "", err
	}
	hook()
	if iStatus != "UNSIGNED" || aStatus != "CURRENT" || current != rev || expired {
		return SignatureDiscarded, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'ACTIVE', permit_jws = $3, signed_at = clock_timestamp()
		WHERE tenant_id = $1 AND permit_id = $2`, f.Tenant, permitID, jws); err != nil {
		return "", err
	}
	return SignaturePersisted, nil
}

// Reserve is the capacity part of evaluation for instruction instr: lock the
// mandate, check available capacity, create an unsigned issuance.
func Reserve(ctx context.Context, tx pgx.Tx, f *Fixture, instr, permitID string, amount int64, hook Hook) (string, error) {
	var available int64
	var status string
	if err := tx.QueryRow(ctx, `SELECT status, available_minor FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 FOR UPDATE`,
		f.Tenant, f.Mandate).Scan(&status, &available); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`, f.Tenant, instr); err != nil {
		return "", err
	}
	hook()
	if status != "ACTIVE" {
		return AuthorityRevoked, nil
	}
	if available < amount {
		return CapacityExhausted, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.issuances (tenant_id, permit_id, reservation_id, operation_id, instruction_id, action_revision,
		mandate_id, control_epoch, amount_minor, status, permit_canonical, permit_digest, issued_at, expires_at)
		SELECT $1, $2, 'res-' || $2, 'op-' || $2, $3, 1, $4, m.control_epoch, $5, 'UNSIGNED', '\x7b7d'::bytea, repeat('d', 64), c.ts, c.ts + interval '5 minutes'
		FROM steelmatter.mandates m, (SELECT clock_timestamp() AS ts) c WHERE m.tenant_id = $1 AND m.mandate_id = $4`, f.Tenant, permitID, instr, f.Mandate, amount); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET available_minor = available_minor - $3, reserved_minor = reserved_minor + $3
		WHERE tenant_id = $1 AND mandate_id = $2`, f.Tenant, f.Mandate, amount); err != nil {
		return "", err
	}
	return Reserved, nil
}
