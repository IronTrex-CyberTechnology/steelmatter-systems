package engine

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// admission carries the checks shared by R0 admission (Admit) and R1
// dispatch intent (Dispatch). Both run the same authorisation gate; they
// differ only in what they commit afterwards.
type admission struct {
	permit       *contract.Permit
	verified     *contract.Verified
	action       *contract.Action
	actionDigest string
	canonical    []byte // canonical action bytes

	// Set inside the transaction by lockForAdmission and verifyLiveAdmission.
	mandate  *mandateRow
	issuance *issuanceRow
	current  int64
}

// preAdmit runs the transport and holder checks that need no lock.
func (e *Engine) preAdmit(p auth.Principal, actionJSON []byte, permitJWS string) (*admission, error) {
	if p.Kind != auth.Actor {
		return nil, fault("FORBIDDEN", "only the assigned actor can admit an operation")
	}
	// Transport and holder checks before any lock.
	verified, verr := contract.Verify(permitJWS, contract.KindPermit, e.Trust)
	if verr != nil {
		return nil, fault(string(verr.Code), "permit: %s", verr.Detail)
	}
	permit := verified.Value.(*contract.Permit)
	if verified.SignedKey.RetiredAt(e.Now()) {
		return nil, fault("JWS_KEY_UNTRUSTED", "permit signing key is retired; re-evaluate for a new permit")
	}
	av, verr := contract.Validate(contract.KindAction, actionJSON)
	if verr != nil {
		return nil, fault(string(verr.Code), "action: %s", verr.Detail)
	}
	action := av.(*contract.Action)
	actionCanonical, err := contract.Canonicalize(actionJSON)
	if err != nil {
		return nil, contractFault(err)
	}
	actionDigest := contract.Digest(contract.KindAction, actionCanonical)

	if permit.TenantID != p.Tenant || action.TenantID != p.Tenant {
		// Inaccessible identity: no disclosure (document 03 errors, D-5).
		return nil, fault("NOT_FOUND", "operation not found")
	}
	if permit.ActorID != p.ID || action.ActorID != p.ID {
		return nil, fault("ACTOR_MISMATCH", "permit or action names a different actor")
	}
	if permit.Audience != e.Cfg.Audience || action.Audience != e.Cfg.Audience {
		return nil, fault("AUDIENCE_MISMATCH", "permit is for a different destination")
	}
	if permit.InstructionID != action.InstructionID || permit.ActionRevision != action.ActionRevision ||
		permit.ActionDigest != actionDigest || permit.AmountMinor != action.AmountMinor {
		return nil, fault("ACTION_CHANGED", "presented action does not match the permitted action digest")
	}

	return &admission{permit: permit, verified: verified, action: action, actionDigest: actionDigest, canonical: actionCanonical}, nil
}

// lockForAdmission locks the mandate and instruction and checks the caller's
// current entitlement. Locks are taken mandate first, as everywhere else.
func (e *Engine) lockForAdmission(ctx context.Context, tx pgx.Tx, p auth.Principal, a *admission) error {
	permit := a.permit
	if err := e.checkClock(ctx, tx); err != nil {
		return err
	}
	m, err := lockMandate(ctx, tx, p.Tenant, permit.MandateID, "FOR UPDATE")
	if err != nil {
		return err
	}
	var current int64
	var actor, sourceSystem string
	err = tx.QueryRow(ctx, `SELECT current_revision, actor_id, source_system FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`,
		p.Tenant, permit.InstructionID).Scan(&current, &actor, &sourceSystem)
	if errors.Is(err, pgx.ErrNoRows) {
		return fault("NOT_FOUND", "instruction not found")
	}
	if err != nil {
		return err
	}
	if actor != p.ID {
		return fault("ACTOR_MISMATCH", "caller is not the instruction's assigned actor")
	}
	if err := currentEntitlement(ctx, tx, p, sourceSystem, permit.MandateID, m, actor); err != nil {
		return err
	}

	a.mandate, a.current = m, current
	return nil
}

// verifyLiveAdmission locks the action revision, issuance and protected
// supplier record, reads time after every lock wait, and refuses anything no
// longer current.
func (e *Engine) verifyLiveAdmission(ctx context.Context, tx pgx.Tx, p auth.Principal, a *admission) error {
	permit, verified, actionDigest, m, current := a.permit, a.verified, a.actionDigest, a.mandate, a.current
	var aStatus, storedDigest, supplier string
	var supplierRev int64
	err := tx.QueryRow(ctx, `SELECT status, action_digest, supplier_id, supplier_revision FROM steelmatter.action_revisions
		WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
		p.Tenant, permit.InstructionID, permit.ActionRevision).Scan(&aStatus, &storedDigest, &supplier, &supplierRev)
	if errors.Is(err, pgx.ErrNoRows) {
		return fault("REVISION_STALE", "action revision not found")
	}
	if err != nil {
		return err
	}
	is, err := scanIssuance(tx.QueryRow(ctx, `SELECT `+issuanceCols+` FROM steelmatter.issuances WHERE tenant_id = $1 AND permit_id = $2 FOR UPDATE`,
		p.Tenant, permit.PermitID))
	if errors.Is(err, pgx.ErrNoRows) {
		return fault("PERMIT_NOT_ACTIVE", "permit is not known to this service")
	}
	if err != nil {
		return err
	}
	var liveSupplierRev int64
	var supplierStatus string
	if err := tx.QueryRow(ctx, `SELECT revision, status FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2 FOR SHARE`,
		p.Tenant, supplier).Scan(&liveSupplierRev, &supplierStatus); err != nil {
		return err
	}
	if err := failpoint.Inject(ctx, failpoint.AdmissionAfterLocks); err != nil {
		return err
	}

	// Time is read fresh here, after every lock wait and stall, not when
	// the rows were locked (I2). The consuming UPDATE below re-guards it.
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() >= m.expires_at, clock_timestamp() >= i.expires_at
		FROM steelmatter.mandates m, steelmatter.issuances i
		WHERE m.tenant_id = $1 AND m.mandate_id = $2 AND i.tenant_id = $1 AND i.permit_id = $3`,
		p.Tenant, permit.MandateID, permit.PermitID).Scan(&m.Expired, &is.Expired); err != nil {
		return err
	}
	switch {
	case m.Status != "ACTIVE":
		return fault("AUTHORITY_REVOKED", "mandate is revoked")
	case m.Expired:
		return fault("MANDATE_EXPIRED", "mandate has expired")
	case permit.ControlEpoch != m.Epoch || is.Epoch != m.Epoch:
		return fault("AUTHORITY_REVOKED", "mandate control epoch changed")
	case permit.MandateRevision != m.Revision || permit.MandateDigest != m.Digest:
		return fault("AUTHORITY_REVOKED", "mandate revision changed")
	case current != permit.ActionRevision || aStatus != "CURRENT":
		return fault("REVISION_STALE", "permit is for a superseded action revision")
	case storedDigest != actionDigest:
		return fault("ACTION_CHANGED", "presented action differs from the stored revision")
	case is.Digest != verified.Digest:
		return fault("PERMIT_NOT_ACTIVE", "permit does not match the stored issuance")
	case is.Status != "ACTIVE":
		return fault("PERMIT_NOT_ACTIVE", "permit is %s", is.Status)
	case is.Expired:
		return fault("PERMIT_EXPIRED", "permit has expired")
	case liveSupplierRev != supplierRev || supplierStatus != "ACTIVE":
		return fault("SUPPLIER_REVISION_STALE", "protected supplier revision changed; amend and re-evaluate")
	}

	a.issuance = is
	return nil
}
