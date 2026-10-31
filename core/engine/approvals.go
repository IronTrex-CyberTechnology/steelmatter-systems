package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
)

// Exact-action human approval (T17-T19, R1 decision 9, migration 0017).
//
// A tenant's approval policy names the amount above which an action needs an
// approval before a permit is issued. An approval binds the exact action
// digest, the protected supplier revision and the mandate revision and
// control epoch. Evaluation accepts the latest decision only while all of
// these are unchanged and the approver is still entitled (T17, T18). The
// approver may not be the issuer who requested the action or the actor who
// would execute it (T18). An approval reserves nothing: capacity is still
// checked under the mandate lock when the permit is issued, so approvals
// that complete together never exceed the allowance (T19).

// Approval decisions and deny reasons.
const (
	ApprovalApproved = "APPROVED"
	ApprovalRejected = "REJECTED"

	DenyApprovalRequired = "APPROVAL_REQUIRED"
	DenyApprovalStale    = "APPROVAL_STALE"
)

// ApprovalRequest is one approver's decision on one exact action revision.
type ApprovalRequest struct {
	InstructionID  string
	ActionRevision int64
	ActionDigest   string
	Approve        bool
	Note           string
}

// ApprovalView is the recorded decision.
type ApprovalView struct {
	ApprovalID     string `json:"approval_id"`
	InstructionID  string `json:"instruction_id"`
	ActionRevision int64  `json:"action_revision"`
	ActionDigest   string `json:"action_digest"`
	Decision       string `json:"decision"`
	ApprovedBy     string `json:"approver_id"`
}

// RecordApproval records an entitled approver's decision on the current
// action revision of an instruction. It refuses an approver who is the
// issuer or the actor of the instruction, an approver without approval
// rights, and a decision on anything but the current action digest.
func (e *Engine) RecordApproval(ctx context.Context, p auth.Principal, r ApprovalRequest) (*ApprovalView, error) {
	if p.Kind != auth.Operator && p.Kind != auth.Owner {
		return nil, fault("FORBIDDEN", "only an entitled approver can approve an action")
	}
	var mandateID string
	err := e.Pool.QueryRow(ctx, `SELECT mandate_id FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`,
		p.Tenant, r.InstructionID).Scan(&mandateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fault("NOT_FOUND", "instruction not found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	decision := ApprovalRejected
	if r.Approve {
		decision = ApprovalApproved
	}
	view := &ApprovalView{ApprovalID: newID("apr"), InstructionID: r.InstructionID, ActionRevision: r.ActionRevision,
		ActionDigest: r.ActionDigest, Decision: decision, ApprovedBy: p.ID}
	err = e.inTx(ctx, "", func(tx pgx.Tx) error {
		m, err := lockMandate(ctx, tx, p.Tenant, mandateID, "FOR SHARE")
		if err != nil {
			return err
		}
		var actor, issuer string
		var current int64
		if err := tx.QueryRow(ctx, `SELECT actor_id, issuer_principal, current_revision FROM steelmatter.instructions
			WHERE tenant_id = $1 AND instruction_id = $2 FOR SHARE`, p.Tenant, r.InstructionID).Scan(&actor, &issuer, &current); err != nil {
			return err
		}
		entitled, err := approverEntitled(ctx, tx, p.Tenant, p.ID)
		if err != nil {
			return err
		}
		if !entitled {
			return fault("FORBIDDEN", "principal holds no approval rights in this tenant")
		}
		if p.ID == issuer || p.ID == actor {
			return fault("FORBIDDEN", "separation of duties: the issuer or actor of an action cannot approve it")
		}
		var status, digest, supplier string
		var supplierRev, amount int64
		err = tx.QueryRow(ctx, `SELECT status, action_digest, supplier_id, supplier_revision, amount_minor FROM steelmatter.action_revisions
			WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR SHARE`, p.Tenant, r.InstructionID, r.ActionRevision).
			Scan(&status, &digest, &supplier, &supplierRev, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("REVISION_STALE", "action revision %d does not exist", r.ActionRevision)
		}
		if err != nil {
			return err
		}
		switch {
		case status == "COMMITTED":
			return fault("ALREADY_COMMITTED", "instruction is already committed")
		case status != "CURRENT" || current != r.ActionRevision:
			return fault("REVISION_STALE", "action revision %d is not current (%d)", r.ActionRevision, current)
		case digest != r.ActionDigest:
			return fault("ACTION_CHANGED", "the approval names action digest %s; the current action is %s", r.ActionDigest, digest)
		case m.Status != "ACTIVE":
			return fault("AUTHORITY_REVOKED", "mandate is revoked")
		}
		var liveRev int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2 FOR SHARE`,
			p.Tenant, supplier).Scan(&liveRev); err != nil {
			return err
		}
		if liveRev != supplierRev {
			return fault("SUPPLIER_REVISION_STALE", "protected supplier revision changed; amend before approving")
		}
		var note any
		if r.Note != "" {
			note = r.Note
		}
		_, err = tx.Exec(ctx, `INSERT INTO steelmatter.approvals (approval_id, tenant_id, instruction_id, action_revision, action_digest,
			supplier_revision, mandate_id, mandate_revision, control_epoch, amount_minor, approver_principal, decision, note, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, clock_timestamp())`,
			view.ApprovalID, p.Tenant, r.InstructionID, r.ActionRevision, digest, supplierRev, mandateID, m.Revision, m.Epoch, amount,
			p.ID, decision, note)
		return err
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

func approverEntitled(ctx context.Context, tx pgx.Tx, tenant, principal string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT e.enabled AND p.enabled AND p.tenant_id = e.tenant_id FROM steelmatter.approver_entitlements e
		JOIN steelmatter.principals p ON p.principal_id = e.approver_principal
		WHERE e.tenant_id = $1 AND e.approver_principal = $2`, tenant, principal).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// approvalDenial returns the deny reason and message when the tenant's
// policy requires an approval for this action and no current one exists, or
// "" when the action may proceed. Called by Evaluate under the mandate,
// instruction and action revision locks.
func approvalDenial(ctx context.Context, tx pgx.Tx, tenant, instructionID string, revision int64, actionDigest string,
	amount, liveSupplierRev int64, m *mandateRow, actor string) (reason, msg string, err error) {
	var above int64
	err = tx.QueryRow(ctx, `SELECT approval_above_minor FROM steelmatter.approval_policies WHERE tenant_id = $1`, tenant).Scan(&above)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil || amount <= above {
		return "", "", err
	}
	var digest, approver, decision, issuer string
	var supplierRev, mandateRev, epoch int64
	err = tx.QueryRow(ctx, `SELECT a.action_digest, a.supplier_revision, a.mandate_revision, a.control_epoch, a.approver_principal, a.decision,
		i.issuer_principal
		FROM steelmatter.approvals a JOIN steelmatter.instructions i ON i.tenant_id = a.tenant_id AND i.instruction_id = a.instruction_id
		WHERE a.tenant_id = $1 AND a.instruction_id = $2 AND a.action_revision = $3
		ORDER BY a.decided_at DESC, a.approval_id DESC LIMIT 1`, tenant, instructionID, revision).
		Scan(&digest, &supplierRev, &mandateRev, &epoch, &approver, &decision, &issuer)
	if errors.Is(err, pgx.ErrNoRows) {
		return DenyApprovalRequired, fmt.Sprintf("actions above %d need an approval of this exact action", above), nil
	}
	if err != nil {
		return "", "", err
	}
	if decision != ApprovalApproved {
		return DenyApprovalRequired, "the latest decision rejected this action; a fresh approval is required", nil
	}
	if approver == issuer || approver == actor {
		return DenyApprovalRequired, "separation of duties: the approval was given by the issuer or actor", nil
	}
	switch {
	case digest != actionDigest:
		return DenyApprovalStale, "the approval was given for another action digest; a fresh decision is required", nil
	case supplierRev != liveSupplierRev:
		return DenyApprovalStale, "the protected supplier changed after the approval; a fresh decision is required", nil
	case mandateRev != m.Revision || epoch != m.Epoch:
		return DenyApprovalStale, "the mandate changed after the approval; a fresh decision is required", nil
	}
	entitled, err := approverEntitled(ctx, tx, tenant, approver)
	if err != nil {
		return "", "", err
	}
	if !entitled {
		return DenyApprovalStale, "the approver no longer holds approval rights; a fresh decision is required", nil
	}
	return "", "", nil
}
