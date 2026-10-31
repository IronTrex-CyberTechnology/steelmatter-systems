package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// HELD findings and repairs (D5 decision, Aakhil, 28 Oct 2026): Uday
// records HELD findings and prepares repair evidence, Kaif reviews it, and
// Aakhil confirms repairs after the recovery checks pass. An operator note
// alone cannot release budget: only ConfirmRepair moves a HELD or MISMATCH
// amount, and only after all three steps are in the anchor.

// Repair outcomes.
const (
	RepairCommit  = "COMMIT"  // the earlier order is established: unresolved -> committed
	RepairRelease = "RELEASE" // no order exists or can exist: unresolved -> available
	// RepairAnchor re-appends a missing DISPATCH_INTENT to an anchor found
	// behind the engine database (R1 decision 4). It moves no budget and goes
	// through the same three people: prepare, review, ConfirmAnchorRepair.
	RepairAnchor = "ANCHOR_REPAIR"
)

// releaseProof (R1 decision 3, R1-DEF-08): HELD money is released only with
// machine proof that no purchase occurred and that the reference is
// permanently blocked. Human approval alone is not enough. The proof is the
// anchor's destination stream, written by the module's own identity:
//
//   - any CREATE_INTENT or CREATE_DONE means a create was attempted, so
//     whether a purchase occurred cannot be proven: RELEASE is refused and
//     the money stays HELD (only COMMIT, or nothing, is possible);
//   - a BLOCK record makes the reference permanently blocked: the anchor
//     refuses any later CREATE_INTENT for it, even after an Odoo restore.
//
// When there is no BLOCK yet and connector c is given, the engine fences the
// reference first and reads the proof again. Every HELD produced by today's
// settle path carries a create attempt, so in practice RELEASE is refused for
// them; it succeeds only for an operation the anchor shows was never sent.
func (e *Engine) releaseProof(ctx context.Context, c connector.Connector, opRef string) error {
	p, err := e.Anchor.ProofFor(ctx, e.Cfg.CompanyRef, opRef)
	if err != nil {
		return dependency("recovery anchor", err)
	}
	if p.CreateIntent || p.CreateDone != nil {
		return fault("FORBIDDEN", "RELEASE_NOT_PROVEN: the recovery anchor records a create attempt for %s, so no purchase cannot be proven; the amount stays HELD", opRef)
	}
	if p.Blocked {
		return nil
	}
	if c == nil {
		return nil // checked again, with a fence, at confirmation
	}
	if e.Cfg.ObserveOnly {
		return fault("FORBIDDEN", "OBSERVE_ONLY: %s has no BLOCK record and this deployment never writes to the destination; "+
			"a deployment with write authority must fence it before a release", opRef)
	}
	if _, err := c.Fence(ctx, opRef, 0); err != nil {
		return dependency("destination", err)
	}
	if p, err = e.Anchor.ProofFor(ctx, e.Cfg.CompanyRef, opRef); err != nil {
		return dependency("recovery anchor", err)
	}
	switch {
	case p.CreateIntent || p.CreateDone != nil:
		return fault("FORBIDDEN", "RELEASE_NOT_PROVEN: the recovery anchor records a create attempt for %s; the amount stays HELD", opRef)
	case !p.Blocked:
		return fault("FORBIDDEN", "RELEASE_NOT_PROVEN: %s is not permanently blocked (no BLOCK record in the recovery anchor); the amount stays HELD", opRef)
	}
	return nil
}

// FindingRecord is the OPERATOR_FINDING payload.
type FindingRecord struct {
	Finding     string `json:"finding"`
	EvidenceRef string `json:"evidence_ref"`
	RecordedBy  string `json:"recorded_by"`
	Status      string `json:"status"`
}

// RepairEvidenceRecord is the REPAIR_EVIDENCE payload.
type RepairEvidenceRecord struct {
	RepairID    string `json:"repair_id"`
	Outcome     string `json:"outcome"`
	EvidenceRef string `json:"evidence_ref"`
	Summary     string `json:"summary"`
	PreparedBy  string `json:"prepared_by"`
}

// RepairReviewRecord is the REPAIR_REVIEWED payload.
type RepairReviewRecord struct {
	RepairID   string `json:"repair_id"`
	Approved   bool   `json:"approved"`
	Note       string `json:"note"`
	ReviewedBy string `json:"reviewed_by"`
}

// RepairConfirmRecord is the REPAIR_CONFIRMED payload.
type RepairConfirmRecord struct {
	RepairID    string `json:"repair_id"`
	Outcome     string `json:"outcome"`
	EvidenceRef string `json:"evidence_ref"`
	PreparedBy  string `json:"prepared_by"`
	ReviewedBy  string `json:"reviewed_by"`
	ConfirmedBy string `json:"confirmed_by"`
	FromStatus  string `json:"from_status"`
	ToStatus    string `json:"to_status"`
	Move        string `json:"move"`
}

func (e *Engine) needAnchor() error {
	if e.Anchor == nil {
		return fault("SERVICE_UNAVAILABLE", "no recovery anchor is configured")
	}
	return nil
}

// heldOp loads an operation that may carry a finding: HELD, MISMATCH, or
// committed with its order missing after a restore.
func (e *Engine) heldOp(ctx context.Context, tenant, opRef string) (engineOp, error) {
	var o engineOp
	err := e.Pool.QueryRow(ctx, `SELECT tenant_id, op_ref, status, COALESCE(flag, ''), mandate_id, amount_minor FROM steelmatter.dispatches
		WHERE tenant_id = $1 AND op_ref = $2`, tenant, opRef).Scan(&o.tenant, &o.opRef, &o.status, &o.flag, &o.mandate, &o.amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, fault("NOT_FOUND", "operation not found")
	}
	if err != nil {
		return o, classify(err)
	}
	if o.status != DispatchHeld && o.status != DispatchMismatch && o.flag == "" {
		return o, fault("REVISION_STALE", "operation is %s; findings and repairs apply only to HELD, MISMATCH or flagged operations", o.status)
	}
	return o, nil
}

// RecordHeldFinding records a HELD finding (Uday). It never moves budget.
func (e *Engine) RecordHeldFinding(ctx context.Context, p auth.Principal, opRef, finding, evidenceRef string) (int64, error) {
	if err := e.needAnchor(); err != nil {
		return 0, err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return 0, err
	}
	if p.Kind != auth.Operator {
		return 0, fault("FORBIDDEN", "only an operator records HELD findings")
	}
	if finding == "" || evidenceRef == "" {
		return 0, fault("SCHEMA_VIOLATION", "a finding and an evidence reference are required")
	}
	o, err := e.heldOp(ctx, p.Tenant, opRef)
	if err != nil {
		return 0, err
	}
	return e.Anchor.Append(ctx, anchor.OperatorFinding, p.Tenant, opRef,
		FindingRecord{Finding: finding, EvidenceRef: evidenceRef, RecordedBy: p.ID, Status: o.status}, 0)
}

// PrepareRepair records the repair evidence (Uday) for a HELD or MISMATCH
// operation with a recorded finding, proposing COMMIT or RELEASE. It returns
// the repair ID. Nothing moves yet.
func (e *Engine) PrepareRepair(ctx context.Context, p auth.Principal, opRef, outcome, evidenceRef, summary string) (string, error) {
	if err := e.needAnchor(); err != nil {
		return "", err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return "", err
	}
	if p.Kind != auth.Operator {
		return "", fault("FORBIDDEN", "only an operator prepares repair evidence")
	}
	if outcome != RepairCommit && outcome != RepairRelease && outcome != RepairAnchor {
		return "", fault("SCHEMA_VIOLATION", "repair outcome must be COMMIT, RELEASE or ANCHOR_REPAIR")
	}
	if evidenceRef == "" {
		return "", fault("SCHEMA_VIOLATION", "an evidence reference is required")
	}
	if outcome == RepairAnchor {
		return e.prepareAnchorRepair(ctx, p, opRef, evidenceRef, summary)
	}
	o, err := e.heldOp(ctx, p.Tenant, opRef)
	if err != nil {
		return "", err
	}
	if o.status != DispatchHeld && o.status != DispatchMismatch {
		return "", fault("REVISION_STALE", "only HELD or MISMATCH operations have amounts to repair")
	}
	recs, err := e.Anchor.RecordsFor(ctx, p.Tenant, opRef)
	if err != nil {
		return "", dependency("recovery anchor", err)
	}
	if !slices.ContainsFunc(recs, func(r anchor.Record) bool { return r.Type == anchor.OperatorFinding }) {
		return "", fault("REVISION_STALE", "record the HELD finding before preparing a repair")
	}
	if outcome == RepairRelease {
		if err := e.releaseProof(ctx, nil, opRef); err != nil {
			return "", err
		}
	}
	id := newID("rep")
	_, err = e.Anchor.Append(ctx, anchor.RepairEvidence, p.Tenant, opRef,
		RepairEvidenceRecord{RepairID: id, Outcome: outcome, EvidenceRef: evidenceRef, Summary: summary, PreparedBy: p.ID}, 0)
	return id, err
}

type repairState struct {
	evidence          *RepairEvidenceRecord
	review            *RepairReviewRecord
	confirmed         *RepairConfirmRecord
	otherConfirmation bool
}

func (e *Engine) repairState(ctx context.Context, tenant, opRef, repairID string) (repairState, error) {
	var st repairState
	recs, err := e.Anchor.RecordsFor(ctx, tenant, opRef)
	if err != nil {
		return st, dependency("recovery anchor", err)
	}
	for _, r := range recs {
		switch r.Type {
		case anchor.RepairEvidence:
			var ev RepairEvidenceRecord
			if json.Unmarshal(r.Payload, &ev) == nil && ev.RepairID == repairID {
				st.evidence = &ev
			}
		case anchor.RepairReviewed:
			var rv RepairReviewRecord
			if json.Unmarshal(r.Payload, &rv) == nil && rv.RepairID == repairID {
				st.review = &rv // the latest review counts
			}
		case anchor.RepairConfirmed:
			var rc RepairConfirmRecord
			if err := json.Unmarshal(r.Payload, &rc); err != nil {
				return st, err
			}
			if rc.RepairID == repairID {
				st.confirmed = &rc
			} else if rc.RepairID != "" && rc.Outcome != RepairAnchor {
				st.otherConfirmation = true // anchor repairs move no budget
			}
		}
	}
	if st.evidence == nil {
		return st, fault("NOT_FOUND", "repair not found")
	}
	return st, nil
}

// ReviewRepair records Kaif's review. The reviewer must be a configured
// repair reviewer and must not have prepared the evidence.
func (e *Engine) ReviewRepair(ctx context.Context, p auth.Principal, opRef, repairID string, approve bool, note string) error {
	if err := e.needAnchor(); err != nil {
		return err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return err
	}
	if p.Kind != auth.Operator || !slices.Contains(e.RepairReviewers, p.ID) {
		return fault("FORBIDDEN", "only a configured repair reviewer can review repair evidence")
	}
	return e.inTx(ctx, opRef, func(tx pgx.Tx) error {
		if _, _, _, _, err := lockRepairOperation(ctx, tx, p.Tenant, opRef); err != nil {
			return err
		}
		st, err := e.repairState(ctx, p.Tenant, opRef, repairID)
		if err != nil {
			return err
		}
		if st.confirmed != nil || st.otherConfirmation {
			return fault("ALREADY_COMMITTED", "repair already confirmed")
		}
		if st.evidence.PreparedBy == p.ID {
			return fault("FORBIDDEN", "the reviewer must not be the person who prepared the evidence")
		}
		_, err = e.Anchor.Append(ctx, anchor.RepairReviewed, p.Tenant, opRef,
			RepairReviewRecord{RepairID: repairID, Approved: approve, Note: note, ReviewedBy: p.ID}, 0)
		return err
	})
}

// ConfirmRepair durably records the human decision BEFORE applying it to
// the engine. These are separate databases, not an atomic transaction.
// Retries and restore reconciliation replay the same decision idempotently;
// they never ask humans to approve an already-durable decision again.
func (e *Engine) ConfirmRepair(ctx context.Context, p auth.Principal, opRef, repairID string, c connector.Connector) (*DispatchResult, error) {
	if err := e.recoveryReady(c); err != nil {
		return nil, err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return nil, err
	}
	if p.Kind != auth.Operator || !slices.Contains(e.RepairConfirmers, p.ID) {
		return nil, fault("FORBIDDEN", "only a configured repair confirmer can confirm a repair")
	}
	err := e.inTx(ctx, opRef, func(tx pgx.Tx) error {
		mandate, status, amount, applied, err := lockRepairOperation(ctx, tx, p.Tenant, opRef)
		if err != nil {
			return err
		}
		// Read the latest review under the SAME lock used by ReviewRepair.
		st, err := e.repairState(ctx, p.Tenant, opRef, repairID)
		if err != nil {
			return err
		}
		if st.evidence.Outcome == RepairAnchor {
			return fault("REVISION_STALE", "an ANCHOR_REPAIR is confirmed with the anchor-repair endpoint")
		}
		if st.otherConfirmation {
			return fault("ALREADY_COMMITTED", "another repair was already confirmed for this operation")
		}
		if st.confirmed != nil {
			return e.applyConfirmedRepairTx(ctx, tx, p.Tenant, opRef, mandate, status, amount, applied, *st.confirmed)
		}
		if st.review == nil || !st.review.Approved {
			return fault("FORBIDDEN", "the repair evidence has not been approved by a reviewer")
		}
		if p.ID == st.evidence.PreparedBy || p.ID == st.review.ReviewedBy {
			return fault("FORBIDDEN", "the confirmer must be different from the preparer and the reviewer")
		}
		if status != DispatchHeld && status != DispatchMismatch {
			return fault("ALREADY_COMMITTED", "operation is already %s", status)
		}
		if on, _, err := e.writesOn(ctx); err != nil || !on {
			return fault("FORBIDDEN", "recovery checks have not passed: writes are not enabled at this engine's generation")
		}
		v, err := e.readViewWithDB(ctx, c, tx)
		if err != nil {
			return err
		}
		if sig := v.signals(); len(sig) > 0 {
			return fault("FORBIDDEN", "recovery checks have not passed: %s %s", sig[0].Code, sig[0].OpRef)
		}
		ev := st.evidence
		to := DispatchCommitted
		if ev.Outcome == RepairRelease {
			if err := e.releaseProof(ctx, c, opRef); err != nil {
				return err
			}
			to = DispatchFenced
		}
		rc := RepairConfirmRecord{RepairID: repairID, Outcome: ev.Outcome, EvidenceRef: ev.EvidenceRef,
			PreparedBy: ev.PreparedBy, ReviewedBy: st.review.ReviewedBy, ConfirmedBy: p.ID,
			FromStatus: status, ToStatus: to, Move: moveFor(to)}
		if _, err := e.Anchor.Append(ctx, anchor.RepairConfirmed, p.Tenant, opRef, rc, e.WriteGeneration()); err != nil {
			return anchorFault(err)
		}
		if err := failpoint.Inject(ctx, failpoint.RepairAfterDecision); err != nil {
			return err
		}
		return e.applyConfirmedRepairTx(ctx, tx, p.Tenant, opRef, mandate, status, amount, applied, rc)
	})
	if err != nil {
		return nil, err
	}
	return e.GetDispatch(ctx, p.Tenant, opRef)
}

func lockRepairOperation(ctx context.Context, tx pgx.Tx, tenant, opRef string) (mandate, status string, amount int64, applied string, err error) {
	if err = tx.QueryRow(ctx, `SELECT mandate_id FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2`, tenant, opRef).Scan(&mandate); err != nil {
		return
	}
	if _, err = lockMandate(ctx, tx, tenant, mandate, "FOR UPDATE"); err != nil {
		return
	}
	err = tx.QueryRow(ctx, `SELECT status, amount_minor, COALESCE(repair_ref, '') FROM steelmatter.dispatches
		WHERE tenant_id = $1 AND op_ref = $2 FOR UPDATE`, tenant, opRef).Scan(&status, &amount, &applied)
	return
}

func (e *Engine) applyConfirmedRepairTx(ctx context.Context, tx pgx.Tx, tenant, opRef, mandate, status string, amount int64, applied string, rc RepairConfirmRecord) error {
	if rc.RepairID == "" || rc.EvidenceRef == "" || rc.PreparedBy == "" || rc.ReviewedBy == "" || rc.ConfirmedBy == "" ||
		rc.PreparedBy == rc.ReviewedBy || rc.PreparedBy == rc.ConfirmedBy || rc.ReviewedBy == rc.ConfirmedBy ||
		!((rc.Outcome == RepairCommit && rc.ToStatus == DispatchCommitted) || (rc.Outcome == RepairRelease && rc.ToStatus == DispatchFenced)) {
		return fault("SERVICE_UNAVAILABLE", "invalid confirmed repair record; keep budget held")
	}
	if applied == rc.RepairID && status == rc.ToStatus {
		return nil
	}
	if applied == "" && status == rc.ToStatus {
		// Recovery may already have established the same destination outcome.
		// Attach the surviving decision without moving its budget a second time.
		_, err := tx.Exec(ctx, `UPDATE steelmatter.dispatches SET repair_ref = $3
			WHERE tenant_id = $1 AND op_ref = $2`, tenant, opRef, rc.RepairID)
		return err
	}
	if applied != "" || (status != DispatchHeld && status != DispatchMismatch) {
		return fault("REVISION_STALE", "confirmed repair cannot be applied to operation in %s", status)
	}
	move := `UPDATE steelmatter.mandates SET unresolved_minor = unresolved_minor - $3, committed_minor = committed_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`
	if rc.Outcome == RepairRelease {
		move = `UPDATE steelmatter.mandates SET unresolved_minor = unresolved_minor - $3, available_minor = available_minor + $3 WHERE tenant_id = $1 AND mandate_id = $2`
	}
	if err := e.anchorOutcome(ctx, anchor.OutcomeRecord{Tenant: tenant, OpRef: opRef, Destination: e.Cfg.CompanyRef,
		Outcome: "REPAIR_" + rc.Outcome, Status: rc.ToStatus, Move: moveFor(rc.ToStatus), RepairID: rc.RepairID}); err != nil {
		return err
	}
	if err := failpoint.Inject(ctx, failpoint.RepairAfterOutcome); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, move, tenant, mandate, amount); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE steelmatter.dispatches SET status = $3, repair_ref = $4, updated_at = clock_timestamp(), settled_at = clock_timestamp()
		WHERE tenant_id = $1 AND op_ref = $2`, tenant, opRef, rc.ToStatus, rc.RepairID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"op_ref": opRef, "repair_id": rc.RepairID, "outcome": rc.Outcome, "amount_minor": amount,
		"confirmed_by": rc.ConfirmedBy, "reviewed_by": rc.ReviewedBy, "prepared_by": rc.PreparedBy})
	if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.outbox (event_id, tenant_id, event_type, aggregate_id, payload, created_at)
		VALUES ($1, $2, 'operation.repaired', $3, $4, clock_timestamp())`, newID("evt"), tenant, opRef, payload); err != nil {
		return err
	}
	return failpoint.Inject(ctx, failpoint.RepairBeforeCommit)
}

// replayConfirmedRepairs runs after unsettled dispatches have been recovered.
// A durable human decision survives a rollback of the engine database.
func (e *Engine) replayConfirmedRepairs(ctx context.Context) ([]string, error) {
	recs, err := e.Anchor.Records(ctx, anchor.RepairConfirmed)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, r := range recs {
		var rc RepairConfirmRecord
		if err := json.Unmarshal(r.Payload, &rc); err != nil {
			return problems, err
		}
		if rc.RepairID == "" || rc.Outcome == RepairAnchor {
			continue
		} // anchor-history repair, not a budget decision
		err := e.inTx(ctx, r.OpRef, func(tx pgx.Tx) error {
			m, status, amount, applied, err := lockRepairOperation(ctx, tx, r.Tenant, r.OpRef)
			if err != nil {
				return err
			}
			return e.applyConfirmedRepairTx(ctx, tx, r.Tenant, r.OpRef, m, status, amount, applied, rc)
		})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: confirmed repair not applied: %v", r.OpRef, err))
		}
	}
	return problems, nil
}

// prepareAnchorRepair records Uday's evidence that an anchor found behind
// the engine database may be repaired for opRef: the engine has the
// dispatch, the anchor has no DISPATCH_INTENT for it.
func (e *Engine) prepareAnchorRepair(ctx context.Context, p auth.Principal, opRef, evidenceRef, summary string) (string, error) {
	var n int
	if err := e.Pool.QueryRow(ctx, `SELECT count(*) FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2`, p.Tenant, opRef).Scan(&n); err != nil {
		return "", classify(err)
	}
	if n == 0 {
		return "", fault("NOT_FOUND", "operation not found")
	}
	recs, err := e.Anchor.RecordsFor(ctx, p.Tenant, opRef)
	if err != nil {
		return "", dependency("recovery anchor", err)
	}
	if slices.ContainsFunc(recs, func(r anchor.Record) bool { return r.Type == anchor.DispatchIntent }) {
		return "", fault("REVISION_STALE", "the recovery anchor already records this operation; nothing to repair")
	}
	id := newID("rep")
	if _, err := e.Anchor.Append(ctx, anchor.RepairEvidence, p.Tenant, opRef,
		RepairEvidenceRecord{RepairID: id, Outcome: RepairAnchor, EvidenceRef: evidenceRef, Summary: summary, PreparedBy: p.ID}, 0); err != nil {
		return "", dependency("recovery anchor", err)
	}
	return id, nil
}

// ConfirmAnchorRepair is Aakhil's confirmation (R1 decision 4) of an
// ANCHOR_REPAIR that Uday prepared and Kaif approved: three different
// people, as for budget repairs. The DISPATCH_INTENT is re-appended from the
// stored dispatch, then a REPAIR_CONFIRMED record names the three people and
// the evidence. A repeat is idempotent. It moves no budget; the resume gate
// still has to pass before writes resume.
func (e *Engine) ConfirmAnchorRepair(ctx context.Context, p auth.Principal, opRef, repairID string) error {
	if err := e.needAnchor(); err != nil {
		return err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return err
	}
	if p.Kind != auth.Operator || !slices.Contains(e.RepairConfirmers, p.ID) {
		return fault("FORBIDDEN", "only a configured repair confirmer can confirm an anchor repair")
	}
	return e.inTx(ctx, opRef, func(tx pgx.Tx) error {
		if _, _, _, _, err := lockRepairOperation(ctx, tx, p.Tenant, opRef); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fault("NOT_FOUND", "operation not found")
			}
			return err
		}
		st, err := e.repairState(ctx, p.Tenant, opRef, repairID)
		if err != nil {
			return err
		}
		if st.evidence.Outcome != RepairAnchor {
			return fault("REVISION_STALE", "repair %s is a %s repair, not an ANCHOR_REPAIR", repairID, st.evidence.Outcome)
		}
		if st.confirmed != nil {
			return nil // already confirmed: the same decision, nothing more to do
		}
		if st.review == nil || !st.review.Approved {
			return fault("FORBIDDEN", "the anchor repair evidence has not been approved by a reviewer")
		}
		if p.ID == st.evidence.PreparedBy || p.ID == st.review.ReviewedBy {
			return fault("FORBIDDEN", "the confirmer must be different from the preparer and the reviewer")
		}
		ir, err := intentFromDispatch(ctx, tx, p.Tenant, opRef)
		if err != nil {
			return err
		}
		if _, err := e.Anchor.Append(ctx, anchor.DispatchIntent, p.Tenant, opRef, ir, 0); err != nil {
			return dependency("recovery anchor", err)
		}
		rc := RepairConfirmRecord{RepairID: repairID, Outcome: RepairAnchor, EvidenceRef: st.evidence.EvidenceRef,
			PreparedBy: st.evidence.PreparedBy, ReviewedBy: st.review.ReviewedBy, ConfirmedBy: p.ID}
		if _, err := e.Anchor.Append(ctx, anchor.RepairConfirmed, p.Tenant, opRef, rc, 0); err != nil {
			return dependency("recovery anchor", err)
		}
		return nil
	})
}

// intentFromDispatch rebuilds the DISPATCH_INTENT from the stored dispatch,
// with the same action facts Dispatch writes (found missing by the first
// engine-plus-Odoo run, case C).
func intentFromDispatch(ctx context.Context, tx pgx.Tx, tenant, opRef string) (anchor.IntentRecord, error) {
	var ir anchor.IntentRecord
	var canonical []byte
	err := tx.QueryRow(ctx, `SELECT d.instruction_id, d.action_revision, d.mandate_id, d.control_epoch, d.company_ref, d.permit_id,
		COALESCE(i.permit_jws, ''), d.permit_digest, d.action_canonical, d.action_digest, d.amount_minor,
		COALESCE(to_char(d.admitted_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		FROM steelmatter.dispatches d JOIN steelmatter.issuances i ON i.tenant_id = d.tenant_id AND i.permit_id = d.permit_id
		WHERE d.tenant_id = $1 AND d.op_ref = $2`, tenant, opRef).Scan(&ir.InstructionID, &ir.ActionRevision, &ir.MandateID, &ir.ControlEpoch,
		&ir.CompanyRef, &ir.PermitID, &ir.PermitJWS, &ir.PermitDigest, &canonical, &ir.ActionDigest, &ir.AmountMinor, &ir.AdmittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ir, fault("NOT_FOUND", "operation not found")
	}
	if err != nil {
		return ir, classify(err)
	}
	ir.Tenant, ir.OpRef, ir.ActionCanonical = tenant, opRef, string(canonical)
	var act struct {
		Currency         string `json:"currency"`
		SupplierID       string `json:"supplier_id"`
		SupplierRevision int64  `json:"supplier_revision"`
		DeliveryID       string `json:"delivery_id"`
	}
	if err := json.Unmarshal(canonical, &act); err != nil {
		return ir, fmt.Errorf("stored action for %s: %w", opRef, err)
	}
	ir.Currency, ir.SupplierRef, ir.SupplierRevision, ir.DeliveryRef = act.Currency, act.SupplierID, act.SupplierRevision, act.DeliveryID
	var src instructionSource
	if err := tx.QueryRow(ctx, `SELECT source_system, source_record_id, issuer_principal, creation_request_digest FROM steelmatter.instructions
		WHERE tenant_id = $1 AND instruction_id = $2`, tenant, ir.InstructionID).Scan(&src.System, &src.RecordID, &src.Issuer, &src.Digest); err != nil {
		return ir, classify(err)
	}
	src.apply(&ir)
	return ir, nil
}
