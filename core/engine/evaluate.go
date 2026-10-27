package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// EvaluateResult is either a permit (Decision PERMIT_ISSUED) or a policy
// denial (Decision DENY). Created distinguishes 201 from 200.
type EvaluateResult struct {
	Decision       string          `json:"decision"`
	Permit         json.RawMessage `json:"permit,omitempty"`
	PermitJWS      string          `json:"permit_jws,omitempty"`
	PermitDigest   string          `json:"permit_digest,omitempty"`
	Action         json.RawMessage `json:"action,omitempty"`
	ActionDigest   string          `json:"action_digest,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
	InstructionID  string          `json:"instruction_id,omitempty"`
	ActionRevision int64           `json:"action_revision,omitempty"`
	Created        bool            `json:"-"`
}

type issuanceRow struct {
	PermitID    string
	OperationID string
	Status      string
	Canonical   []byte
	Digest      string
	JWS         *string
	Expired     bool
	Epoch       int64
	Amount      int64
	Revision    int64
}

const issuanceCols = `permit_id, operation_id, status, permit_canonical, permit_digest, permit_jws, clock_timestamp() >= expires_at, control_epoch, amount_minor, action_revision`

func scanIssuance(row pgx.Row) (*issuanceRow, error) {
	var i issuanceRow
	if err := row.Scan(&i.PermitID, &i.OperationID, &i.Status, &i.Canonical, &i.Digest, &i.JWS, &i.Expired, &i.Epoch, &i.Amount, &i.Revision); err != nil {
		return nil, err
	}
	return &i, nil
}

func deny(reason, msg, instructionID string, rev int64) *EvaluateResult {
	return &EvaluateResult{Decision: "DENY", Reason: reason, Message: msg, InstructionID: instructionID, ActionRevision: rev}
}

// Evaluate reserves capacity and issues a signed permit for the assigned
// actor (201). An identical retry returns the existing active issuance
// without extending, re-reserving or reactivating it (200), unless its
// signing key has since been retired: then that issuance is released as
// KEY_RETIRED and a new permit is issued with the current key (201,
// ADR-004). Policy refusals
// return DENY (200). Evaluation changes state; it is not a dry run.
func (e *Engine) Evaluate(ctx context.Context, p auth.Principal, instructionID string, revision int64) (*EvaluateResult, error) {
	if p.Kind != auth.Actor {
		return nil, fault("FORBIDDEN", "only the assigned actor can evaluate")
	}
	ctx = failpoint.WithKey(ctx, instructionID)
	var mandateID, actor, sourceSystem string
	err := e.Pool.QueryRow(ctx, `SELECT mandate_id, actor_id, source_system FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`,
		p.Tenant, instructionID).Scan(&mandateID, &actor, &sourceSystem)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fault("NOT_FOUND", "instruction not found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	if actor != p.ID {
		return nil, fault("ACTOR_MISMATCH", "caller is not the instruction's assigned actor")
	}
	// Refuse before reserving anything if no current permit key exists.
	if _, err := e.signer("SM-PERMIT+JWS"); err != nil {
		return nil, err
	}

	// IDs are fixed before the transaction so retries reuse them.
	newPermit := struct{ permit, reservation, operation string }{newID("permit"), newID("res"), newID("op")}
	var res *EvaluateResult
	var toSign *issuanceRow
	var actionCanonical []byte
	var actionDigest string
	// No operation ID exists until this transaction commits, so none is
	// returned if it exhausts its retries (F-18).
	err = e.inTx(ctx, "", func(tx pgx.Tx) error {
		res, toSign = nil, nil
		if err := e.checkClock(ctx, tx); err != nil {
			return err
		}
		m, err := lockMandate(ctx, tx, p.Tenant, mandateID, "FOR UPDATE")
		if err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
			p.Tenant, instructionID).Scan(&current); err != nil {
			return err
		}
		if err := currentEntitlement(ctx, tx, p, sourceSystem, mandateID, m, actor); err != nil {
			return err
		}
		var aStatus, supplier, delivery string
		var supplierRev, amount int64
		err = tx.QueryRow(ctx, `SELECT status, action_canonical, action_digest, supplier_id, supplier_revision, delivery_id, amount_minor
			FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
			p.Tenant, instructionID, revision).Scan(&aStatus, &actionCanonical, &actionDigest, &supplier, &supplierRev, &delivery, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("REVISION_STALE", "action revision %d does not exist", revision)
		}
		if err != nil {
			return err
		}
		live, err := scanIssuance(tx.QueryRow(ctx, `SELECT `+issuanceCols+` FROM steelmatter.issuances
			WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 AND status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED') FOR UPDATE`,
			p.Tenant, instructionID, revision))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var liveSupplierRev int64
		var supplierStatus string
		if err := tx.QueryRow(ctx, `SELECT revision, status FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2 FOR SHARE`,
			p.Tenant, supplier).Scan(&liveSupplierRev, &supplierStatus); err != nil {
			return err
		}

		if aStatus == "COMMITTED" || (live != nil && live.Status == "CONSUMED") {
			return fault("ALREADY_COMMITTED", "instruction is already committed")
		}
		if aStatus != "CURRENT" || current != revision {
			return fault("REVISION_STALE", "action revision %d is not current (%d)", revision, current)
		}
		// Policy denials (no state change).
		if m.Status != "ACTIVE" {
			res = deny("AUTHORITY_REVOKED", "mandate is revoked", instructionID, revision)
			return nil
		}
		if m.Expired {
			res = deny("MANDATE_EXPIRED", "mandate has expired", instructionID, revision)
			return nil
		}
		if !contains(m.Supplier, supplier) {
			res = deny("SUPPLIER_NOT_PERMITTED", "supplier is outside the mandate", instructionID, revision)
			return nil
		}
		if !contains(m.Delivery, delivery) {
			res = deny("DELIVERY_NOT_PERMITTED", "delivery point is outside the mandate", instructionID, revision)
			return nil
		}
		if liveSupplierRev != supplierRev || supplierStatus != "ACTIVE" {
			return fault("SUPPLIER_REVISION_STALE", "protected supplier revision changed; amend and re-evaluate")
		}

		if live != nil {
			usable := live.Status == "ACTIVE" && !live.Expired && live.Epoch == m.Epoch
			switch {
			case usable && e.permitKeyCurrent(*live.JWS):
				// Identical retry: the existing issuance, unchanged.
				res = &EvaluateResult{Decision: "PERMIT_ISSUED", Permit: live.Canonical, PermitJWS: *live.JWS, PermitDigest: live.Digest,
					Action: actionCanonical, ActionDigest: actionDigest}
				return nil
			case live.Status == "UNSIGNED" && !live.Expired && live.Epoch == m.Epoch:
				// Signing was interrupted: complete the same payload (T16).
				toSign = live
				return nil
			case usable:
				// Live but signed by a key admission no longer accepts (retired
				// after rotation): returning it would loop the caller until it
				// expires (SEC-DEF-001). Release it under these locks and issue
				// a new permit with the current key below (ADR-004).
				if err := releaseIssuance(ctx, tx, p.Tenant, mandateID, live, "KEY_RETIRED"); err != nil {
					return err
				}
			default:
				// Expired (or stale epoch) and unused: release it under these
				// locks, then issue a new permit below. The old permission is
				// never reactivated.
				if err := releaseIssuance(ctx, tx, p.Tenant, mandateID, live, "EXPIRED"); err != nil {
					return err
				}
			}
		}

		var available int64
		if err := tx.QueryRow(ctx, `SELECT available_minor FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2`,
			p.Tenant, mandateID).Scan(&available); err != nil {
			return err
		}
		if available < amount {
			res = deny("CAPACITY_EXHAUSTED", "shared allowance is insufficient", instructionID, revision)
			return nil
		}
		var issued, mandateExpires time.Time
		if err := tx.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp()), expires_at FROM steelmatter.mandates
			WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID).Scan(&issued, &mandateExpires); err != nil {
			return err
		}
		expires := issued.Add(e.Cfg.PermitTTL)
		if mandateExpires.Before(expires) {
			expires = mandateExpires.Truncate(time.Second) // never beyond mandate expiry
		}
		if !expires.After(issued) {
			res = deny("MANDATE_EXPIRED", "mandate expires before a permit could be used", instructionID, revision)
			return nil
		}
		permit := contract.Permit{
			SchemaVersion: contract.SchemaVersion, CryptoProfile: contract.CryptoProfile,
			PermitID: newPermit.permit, ReservationID: newPermit.reservation, OperationID: newPermit.operation,
			TenantID: p.Tenant, InstructionID: instructionID, ActionRevision: revision, ActionDigest: actionDigest,
			MandateID: mandateID, MandateRevision: m.Revision, MandateDigest: m.Digest, ControlEpoch: m.Epoch,
			ActorID: actor, Audience: m.Audience, Currency: contract.Currency, AmountMinor: amount,
			IssuedAt: ts(issued), ExpiresAt: ts(expires),
		}
		raw, _ := json.Marshal(permit)
		canonical, pdigest, err := contract.CanonicalDigest(contract.KindPermit, raw)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.issuances (tenant_id, permit_id, reservation_id, operation_id, instruction_id,
			action_revision, mandate_id, control_epoch, amount_minor, status, permit_canonical, permit_digest, issued_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'UNSIGNED', $10, $11, $12, $13)`,
			p.Tenant, permit.PermitID, permit.ReservationID, permit.OperationID, instructionID, revision, mandateID, m.Epoch, amount,
			canonical, pdigest, issued, expires); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET available_minor = available_minor - $3, reserved_minor = reserved_minor + $3
			WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID, amount); err != nil {
			return err
		}
		if err := failpoint.Inject(ctx, failpoint.EvaluateBeforeReserveCommit); err != nil {
			return err
		}
		toSign = &issuanceRow{PermitID: permit.PermitID, OperationID: permit.OperationID, Status: "UNSIGNED", Canonical: canonical, Digest: pdigest, Epoch: m.Epoch, Amount: amount, Revision: revision}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res != nil {
		return res, nil
	}
	if err := failpoint.Inject(ctx, failpoint.EvaluateAfterReserve); err != nil {
		return nil, unavailable(err)
	}
	return e.signAndPersist(ctx, p.Tenant, mandateID, instructionID, toSign, actionCanonical, actionDigest)
}

// signAndPersist signs the stored immutable payload outside any transaction,
// then locks in order and publishes the signature only if the issuance is
// still unsigned, its revision current, the epoch unchanged and unexpired.
func (e *Engine) signAndPersist(ctx context.Context, tenant, mandateID, instructionID string, is *issuanceRow, actionCanonical []byte, actionDigest string) (*EvaluateResult, error) {
	signer, err := e.signer("SM-PERMIT+JWS")
	if err != nil {
		return nil, err
	}
	jws, err := contract.Sign(contract.KindPermit, is.Canonical, signer)
	if err != nil {
		return nil, err
	}
	if err := failpoint.Inject(ctx, failpoint.EvaluateBeforePersist); err != nil {
		return nil, unavailable(err)
	}
	var res *EvaluateResult
	err = e.inTx(ctx, is.OperationID, func(tx pgx.Tx) error {
		m, err := lockMandate(ctx, tx, tenant, mandateID, "FOR UPDATE")
		if err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
			tenant, instructionID).Scan(&current); err != nil {
			return err
		}
		var aStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
			tenant, instructionID, is.Revision).Scan(&aStatus); err != nil {
			return err
		}
		cur, err := scanIssuance(tx.QueryRow(ctx, `SELECT `+issuanceCols+` FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
			tenant, is.PermitID))
		if err != nil {
			return err
		}
		switch {
		case cur.Status == "ACTIVE":
			// A concurrent retry published first; return what is stored.
			res = &EvaluateResult{Decision: "PERMIT_ISSUED", Permit: cur.Canonical, PermitJWS: *cur.JWS, PermitDigest: cur.Digest,
				Action: actionCanonical, ActionDigest: actionDigest}
			return nil
		}
		// The precise reason the issuance can no longer be published (I02 §8 Q5; D-7).
		var reason *string
		if err := tx.QueryRow(ctx, `SELECT release_reason FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`,
			tenant, is.PermitID).Scan(&reason); err != nil {
			return err
		}
		switch {
		case m.Status != "ACTIVE" || cur.Epoch != m.Epoch || (reason != nil && *reason == "REVOKED"):
			return fault("AUTHORITY_REVOKED", "mandate was revoked before the signature was persisted")
		case reason != nil && (*reason == "EXPIRED" || *reason == "RECOVERED_UNUSED"), cur.Expired, m.Expired:
			return fault("PERMIT_EXPIRED", "issuance expired before its signature was persisted")
		case cur.Status != "UNSIGNED" || current != is.Revision || aStatus != "CURRENT":
			return fault("REVISION_STALE", "issuance was invalidated before its signature was persisted")
		}
		tag, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'ACTIVE', permit_jws = $3, signed_at = clock_timestamp()
			WHERE tenant_id = $1 AND permit_id = $2 AND status = 'UNSIGNED' AND clock_timestamp() < expires_at`, tenant, is.PermitID, jws)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fault("PERMIT_EXPIRED", "issuance expired before its signature was persisted")
		}
		res = &EvaluateResult{Decision: "PERMIT_ISSUED", Permit: cur.Canonical, PermitJWS: jws, PermitDigest: cur.Digest,
			Action: actionCanonical, ActionDigest: actionDigest, Created: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// permitKeyCurrent reports whether admission would still accept the stored
// permit's signature: it verifies under the trust store and its key is not
// retired now. This is the same rule Admit applies.
func (e *Engine) permitKeyCurrent(permitJWS string) bool {
	v, verr := contract.Verify(permitJWS, contract.KindPermit, e.Trust)
	return verr == nil && !v.SignedKey.RetiredAt(e.Now())
}

func releaseIssuance(ctx context.Context, tx pgx.Tx, tenant, mandateID string, is *issuanceRow, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = $3, released_at = clock_timestamp()
		WHERE tenant_id = $1 AND permit_id = $2`, tenant, is.PermitID, reason); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
		WHERE tenant_id = $1 AND mandate_id = $2`, tenant, mandateID, is.Amount)
	return err
}

// RecoverUnsigned completes unsigned issuances older than RecoveryAge that
// are still valid by signing their stored payload (T16). Expired unsigned
// issuances are left to CleanupExpired. Returns the number published.
func (e *Engine) RecoverUnsigned(ctx context.Context) (int, error) {
	rows, err := e.Pool.Query(ctx, `SELECT i.tenant_id, i.mandate_id, i.instruction_id, i.permit_id, i.operation_id, a.action_canonical, a.action_digest
		FROM steelmatter.issuances i JOIN steelmatter.action_revisions a
		  ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.action_revision
		WHERE i.status = 'UNSIGNED' AND i.expires_at > clock_timestamp() AND i.issued_at <= clock_timestamp() - $1::double precision * interval '1 second'
		ORDER BY i.tenant_id, i.permit_id LIMIT 100`, e.Cfg.RecoveryAge.Seconds())
	if err != nil {
		return 0, err
	}
	type cand struct {
		tenant, mandate, instruction, permit, operation, digest string
		action                                                  []byte
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.tenant, &c.mandate, &c.instruction, &c.permit, &c.operation, &c.action, &c.digest); err != nil {
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
		var is issuanceRow
		if err := e.Pool.QueryRow(ctx, `SELECT permit_canonical, permit_digest, control_epoch, amount_minor, action_revision
			FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2`, c.tenant, c.permit).
			Scan(&is.Canonical, &is.Digest, &is.Epoch, &is.Amount, &is.Revision); err != nil {
			return n, err
		}
		is.PermitID, is.OperationID, is.Status = c.permit, c.operation, "UNSIGNED"
		res, err := e.signAndPersist(ctx, c.tenant, c.mandate, c.instruction, &is, c.action, c.digest)
		if err != nil {
			if f, ok := AsFault(err); ok && f.Code != "SERVICE_UNAVAILABLE" && f.Code != "RETRIES_EXHAUSTED" && f.Code != "INTERNAL_ERROR" {
				continue // invalidated or expired meanwhile; cleanup releases it
			}
			return n, err
		}
		if res.Created {
			n++
		}
	}
	return n, nil
}
