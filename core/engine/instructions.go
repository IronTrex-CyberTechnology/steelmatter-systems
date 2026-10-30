package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
)

// ProposedAction holds the material fields an issuer may propose.
type ProposedAction struct {
	SupplierID string          `json:"supplier_id"`
	DeliveryID string          `json:"delivery_id"`
	Currency   string          `json:"currency"`
	Lines      []contract.Line `json:"lines"`
	// TaxMinor is the issuer's tax amount for lines with tax_ids (D1 v1.4,
	// R1 decision 1). The engine holds untaxed + tax; the destination
	// recomputes the tax at create and refuses any difference.
	TaxMinor *int64 `json:"tax_minor,omitempty"`
	// DeliveryRevision is read by the engine from the destination, never
	// proposed (R1 decision 2); it is not part of the request identity.
	DeliveryRevision *int64 `json:"-"`
}

// InstructionRequest is the trusted issuer's creation request.
// ClientRequestID is correlation only and never part of identity.
type InstructionRequest struct {
	SourceSystem    string
	SourceRecordID  string
	MandateID       string
	Proposed        ProposedAction
	ClientRequestID string
}

// InstructionView is returned by create.
type InstructionView struct {
	InstructionID  string          `json:"instruction_id"`
	SourceSystem   string          `json:"source_system"`
	SourceRecordID string          `json:"source_record_id"`
	MandateID      string          `json:"mandate_id"`
	ActorID        string          `json:"actor_id"`
	ActionRevision int64           `json:"action_revision"`
	Action         json.RawMessage `json:"action"`
	ActionDigest   string          `json:"action_digest"`
	Status         string          `json:"status"`
}

// AmendmentRequest is the issuer's revision request.
type AmendmentRequest struct {
	AmendmentID      string
	ExpectedRevision int64
	Proposed         ProposedAction
}

// AmendmentView is returned by amend.
type AmendmentView struct {
	AmendmentID          string          `json:"amendment_id"`
	InstructionID        string          `json:"instruction_id"`
	PreviousRevision     int64           `json:"previous_action_revision"`
	ActionRevision       int64           `json:"action_revision"`
	Action               json.RawMessage `json:"action"`
	ActionDigest         string          `json:"action_digest"`
	InvalidatedPermitIDs []string        `json:"invalidated_permit_ids"`
	ReleasedMinor        int64           `json:"released_minor"`
}

type mandateRow struct {
	Status   string
	Epoch    int64
	Expired  bool
	Audience string
	Actors   []string
	Supplier []string
	Delivery []string
	Owner    string
	Revision int64
	Digest   string
}

func lockMandate(ctx context.Context, tx pgx.Tx, tenant, id, mode string) (*mandateRow, error) {
	var m mandateRow
	err := tx.QueryRow(ctx, `SELECT status, control_epoch, clock_timestamp() >= expires_at, audience, actor_ids, supplier_ids, delivery_ids,
		owner_principal, revision, mandate_digest FROM steelmatter.mandates WHERE tenant_id = $1 AND mandate_id = $2 `+mode, tenant, id).
		Scan(&m.Status, &m.Epoch, &m.Expired, &m.Audience, &m.Actors, &m.Supplier, &m.Delivery, &m.Owner, &m.Revision, &m.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fault("NOT_FOUND", "mandate not found")
	}
	return &m, err
}

func (m *mandateRow) usable() error {
	if m.Status != "ACTIVE" {
		return fault("AUTHORITY_REVOKED", "mandate is revoked")
	}
	if m.Expired {
		return fault("MANDATE_EXPIRED", "mandate has expired")
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func issuerEntitled(ctx context.Context, q pgx.Tx, p auth.Principal, sourceSystem string) (bool, error) {
	if p.Kind != auth.Issuer {
		return false, nil
	}
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM steelmatter.issuer_entitlements
		WHERE tenant_id = $1 AND issuer_principal = $2 AND source_system = $3)`, p.Tenant, p.ID, sourceSystem).Scan(&ok)
	return ok, err
}

// resolveActor selects the executing actor from the trusted source-workflow
// assignment. The issuer identity is never used (T05, T12).
func resolveActor(ctx context.Context, tx pgx.Tx, tenant, sourceSystem, mandateID string, m *mandateRow) (string, error) {
	rows, err := tx.Query(ctx, `SELECT wa.actor_id, wa.enabled, p.enabled, p.kind, p.tenant_id
		FROM steelmatter.workflow_assignments wa JOIN steelmatter.principals p ON p.principal_id = wa.actor_id
		WHERE wa.tenant_id = $1 AND wa.source_system = $2 AND wa.mandate_id = $3 ORDER BY wa.assignment_id`, tenant, sourceSystem, mandateID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	enabled := map[string]bool{}
	var actor string
	var anyDisabled, principalOK bool
	for rows.Next() {
		var id, kind, ptenant string
		var waEnabled, pEnabled bool
		if err := rows.Scan(&id, &waEnabled, &pEnabled, &kind, &ptenant); err != nil {
			return "", err
		}
		if !waEnabled {
			anyDisabled = true
			continue
		}
		enabled[id] = true
		actor = id
		principalOK = pEnabled && kind == auth.Actor && ptenant == tenant
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(enabled) == 0 && anyDisabled:
		return "", fault("ACTOR_ASSIGNMENT_INVALID", "actor assignment is disabled")
	case len(enabled) == 0:
		return "", fault("ACTOR_ASSIGNMENT_INVALID", "no actor assignment for this source workflow and mandate")
	case len(enabled) > 1:
		return "", fault("ACTOR_ASSIGNMENT_INVALID", "actor assignment is ambiguous")
	case !principalOK:
		return "", fault("ACTOR_ASSIGNMENT_INVALID", "assigned actor is not an enabled actor in this tenant")
	case !contains(m.Actors, actor):
		return "", fault("ACTOR_ASSIGNMENT_INVALID", "assigned actor is outside the mandate")
	}
	return actor, nil
}

// currentEntitlement re-reads the trusted source-workflow assignment under
// the instruction lock and requires it to still select the stored actor, who
// must be the caller (documents 02/03: "current entitlement"; F-04).
// Disabling or changing the assignment therefore stops further permits and
// admissions for that instruction.
func currentEntitlement(ctx context.Context, tx pgx.Tx, p auth.Principal, sourceSystem, mandateID string, m *mandateRow, actor string) error {
	resolved, err := resolveActor(ctx, tx, p.Tenant, sourceSystem, mandateID, m)
	if f, ok := AsFault(err); ok && f.Code == "ACTOR_ASSIGNMENT_INVALID" {
		return fault("ACTOR_MISMATCH", "caller's actor assignment is no longer current")
	}
	if err != nil {
		return err
	}
	if resolved != actor || p.ID != actor {
		return fault("ACTOR_MISMATCH", "caller's actor assignment is no longer current")
	}
	return nil
}

// buildAction normalises the proposal, reads the protected supplier revision
// (FOR SHARE, last in lock order) and returns the canonical action.
func buildAction(ctx context.Context, tx pgx.Tx, tenant, instructionID string, revision int64, actor string, m *mandateRow, pa ProposedAction) (canonical []byte, digest string, a *contract.Action, err error) {
	if pa.Currency != contract.Currency {
		return nil, "", nil, fault("UNSUPPORTED_CURRENCY", "only INR is supported")
	}
	lines, cerr := contract.NormaliseLines(pa.Lines)
	if cerr != nil {
		return nil, "", nil, fault(string(cerr.Code), "%s", cerr.Detail)
	}
	total, cerr := contract.LineTotal(lines)
	if cerr != nil {
		return nil, "", nil, fault(string(cerr.Code), "%s", cerr.Detail)
	}
	// D1 v1.4 (R1 decision 1): the exact taxes and the full total are
	// authorised. Taxes without a tax amount, or a tax amount without taxes,
	// are refused; nothing is ever dropped or recomputed here.
	taxed := false
	for _, l := range lines {
		taxed = taxed || len(l.TaxIDs) > 0
	}
	var untaxed, tax *int64
	amount := total
	switch {
	case taxed && pa.TaxMinor == nil:
		return nil, "", nil, fault("SCHEMA_VIOLATION", "lines with tax_ids require tax_minor")
	case !taxed && pa.TaxMinor != nil:
		return nil, "", nil, fault("SCHEMA_VIOLATION", "tax_minor requires at least one line with tax_ids")
	case taxed:
		if *pa.TaxMinor < 0 || *pa.TaxMinor > contract.MaxSafeInteger-total {
			return nil, "", nil, fault("ARITHMETIC_OVERFLOW", "line total + tax_minor exceeds 2^53-1")
		}
		u, t := total, *pa.TaxMinor
		untaxed, tax, amount = &u, &t, total+t
	}
	if amount < 1 {
		return nil, "", nil, fault("SCHEMA_VIOLATION", "the order total must be at least 1")
	}
	var supplierRev int64
	var supplierStatus string
	err = tx.QueryRow(ctx, `SELECT revision, status FROM target.suppliers WHERE tenant_id = $1 AND supplier_id = $2 FOR SHARE`,
		tenant, pa.SupplierID).Scan(&supplierRev, &supplierStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && supplierStatus != "ACTIVE") {
		return nil, "", nil, fault("SUPPLIER_UNAVAILABLE", "supplier is unknown or suspended")
	}
	if err != nil {
		return nil, "", nil, err
	}
	a = &contract.Action{
		SchemaVersion: contract.SchemaVersion, CryptoProfile: contract.CryptoProfile,
		TenantID: tenant, InstructionID: instructionID, ActionRevision: revision, ActorID: actor,
		Audience: m.Audience, Operation: "purchase_order.issue", SupplierID: pa.SupplierID, SupplierRevision: supplierRev,
		DeliveryID: pa.DeliveryID, Currency: contract.Currency, Lines: lines, AmountMinor: amount,
		AmountUntaxedMinor: untaxed, TaxMinor: tax, DeliveryRevision: pa.DeliveryRevision,
	}
	raw, _ := json.Marshal(a)
	canonical, digest, err = contract.CanonicalDigest(contract.KindAction, raw)
	if err != nil {
		if ce, ok := err.(*contract.Error); ok {
			return nil, "", nil, fault(string(ce.Code), "%s", ce.Detail)
		}
		return nil, "", nil, err
	}
	return canonical, digest, a, nil
}

// requestDigest digests a request with normalised lines, so proposal order
// does not change identity (T40) and client_request_id is excluded (T11).
func requestDigest(v any) string {
	raw, _ := json.Marshal(v)
	canonical, err := contract.Canonicalize(raw)
	if err != nil {
		panic(err)
	}
	return contract.Digest(contract.KindAction, append([]byte("request:"), canonical...))
}

func normalisedProposal(pa ProposedAction) (ProposedAction, error) {
	lines, cerr := contract.NormaliseLines(pa.Lines)
	if cerr != nil {
		return pa, fault(string(cerr.Code), "%s", cerr.Detail)
	}
	pa.Lines = lines
	return pa, nil
}

func insertRevision(ctx context.Context, tx pgx.Tx, a *contract.Action, canonical []byte, digest string) error {
	_, err := tx.Exec(ctx, `INSERT INTO steelmatter.action_revisions (tenant_id, instruction_id, action_revision, status, action_canonical,
		action_digest, supplier_id, supplier_revision, delivery_id, audience, amount_minor, created_at)
		VALUES ($1, $2, $3, 'CURRENT', $4, $5, $6, $7, $8, $9, $10, clock_timestamp())`,
		a.TenantID, a.InstructionID, a.ActionRevision, canonical, digest, a.SupplierID, a.SupplierRevision, a.DeliveryID, a.Audience, a.AmountMinor)
	return err
}

func loadInstruction(ctx context.Context, q pgx.Tx, tenant, id string) (*InstructionView, error) {
	var v InstructionView
	var committed bool
	err := q.QueryRow(ctx, `SELECT i.instruction_id, i.source_system, i.source_record_id, i.mandate_id, i.actor_id, i.current_revision,
		a.action_canonical, a.action_digest, a.status = 'COMMITTED'
		FROM steelmatter.instructions i JOIN steelmatter.action_revisions a
		  ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.current_revision
		WHERE i.tenant_id = $1 AND i.instruction_id = $2`, tenant, id).
		Scan(&v.InstructionID, &v.SourceSystem, &v.SourceRecordID, &v.MandateID, &v.ActorID, &v.ActionRevision, &v.Action, &v.ActionDigest, &committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fault("NOT_FOUND", "instruction not found")
	}
	v.Status = "OPEN"
	if committed {
		v.Status = "COMMITTED"
	}
	return &v, err
}

// CreateInstruction creates a trusted instruction (201), returns the existing
// one for an identical request (200, at its current revision), or conflicts
// on a changed request for the same source record (409 SOURCE_CONFLICT).
func (e *Engine) CreateInstruction(ctx context.Context, p auth.Principal, r InstructionRequest) (*InstructionView, bool, error) {
	norm, err := normalisedProposal(r.Proposed)
	if err != nil {
		return nil, false, err
	}
	digest := requestDigest(map[string]any{"source_system": r.SourceSystem, "source_record_id": r.SourceRecordID, "mandate_id": r.MandateID, "proposed_action": norm})
	instructionID := newID("inst")
	if err := e.requireNoRecovery(ctx); err != nil {
		return nil, false, err
	}
	if err := e.syncSupplierFor(ctx, p, r.Proposed.SupplierID); err != nil {
		return nil, false, err
	}
	if norm.DeliveryRevision, err = e.deliveryRevisionFor(ctx, p, r.Proposed.DeliveryID); err != nil {
		return nil, false, err
	}

	var view *InstructionView
	var created bool
	for attempt := 0; attempt < 2; attempt++ {
		err = e.inTx(ctx, "", func(tx pgx.Tx) error {
			ok, err := issuerEntitled(ctx, tx, p, r.SourceSystem)
			if err != nil {
				return err
			}
			if !ok {
				return fault("FORBIDDEN", "principal is not a trusted issuer for this source system")
			}
			m, err := lockMandate(ctx, tx, p.Tenant, r.MandateID, "FOR SHARE")
			if err != nil {
				return err
			}
			var existingID, existingDigest string
			err = tx.QueryRow(ctx, `SELECT instruction_id, creation_request_digest FROM steelmatter.instructions
				WHERE tenant_id = $1 AND source_system = $2 AND source_record_id = $3 AND superseded_by IS NULL FOR UPDATE`,
				p.Tenant, r.SourceSystem, r.SourceRecordID).Scan(&existingID, &existingDigest)
			if err == nil {
				if existingDigest != digest {
					return fault("SOURCE_CONFLICT", "source record already exists with a different request")
				}
				view, err = loadInstruction(ctx, tx, p.Tenant, existingID)
				created = false
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err := m.usable(); err != nil {
				return err
			}
			actor, err := resolveActor(ctx, tx, p.Tenant, r.SourceSystem, r.MandateID, m)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.instructions (tenant_id, instruction_id, source_system, source_record_id,
				mandate_id, actor_id, issuer_principal, creation_request_digest, current_revision, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, clock_timestamp())`,
				p.Tenant, instructionID, r.SourceSystem, r.SourceRecordID, r.MandateID, actor, p.ID, digest); err != nil {
				if isUnique(err, "instruction_source_unique") {
					return errSourceRace
				}
				return err
			}
			canonical, adigest, a, err := buildAction(ctx, tx, p.Tenant, instructionID, 1, actor, m, norm)
			if err != nil {
				return err
			}
			if err := insertRevision(ctx, tx, a, canonical, adigest); err != nil {
				return err
			}
			created = true
			view = &InstructionView{InstructionID: instructionID, SourceSystem: r.SourceSystem, SourceRecordID: r.SourceRecordID,
				MandateID: r.MandateID, ActorID: actor, ActionRevision: 1, Action: canonical, ActionDigest: adigest, Status: "OPEN"}
			return nil
		})
		// A concurrent identical creation won the unique source key: rerun to
		// return it (or conflict).
		if errors.Is(err, errSourceRace) && attempt == 0 {
			continue
		}
		break
	}
	if errors.Is(err, errSourceRace) {
		err = unavailable(err)
	}
	if err != nil {
		return nil, false, err
	}
	return view, created, nil
}

// errSourceRace signals that a concurrent creation inserted the same source
// key first; the caller reruns once to return or conflict with it.
var errSourceRace = &Fault{Code: "SOURCE_RACE", Message: "concurrent creation of the same source record"}

// AmendInstruction appends an uncommitted revision under the global locks
// (201); an exact amendment_id retry returns the recorded result (200).
func (e *Engine) AmendInstruction(ctx context.Context, p auth.Principal, instructionID string, r AmendmentRequest) (*AmendmentView, bool, error) {
	norm, err := normalisedProposal(r.Proposed)
	if err != nil {
		return nil, false, err
	}
	digest := requestDigest(map[string]any{"amendment_id": r.AmendmentID, "expected_action_revision": r.ExpectedRevision, "proposed_action": norm})
	if err := e.syncSupplierFor(ctx, p, r.Proposed.SupplierID); err != nil {
		return nil, false, err
	}
	if norm.DeliveryRevision, err = e.deliveryRevisionFor(ctx, p, r.Proposed.DeliveryID); err != nil {
		return nil, false, err
	}
	if p.Kind != auth.Issuer {
		return nil, false, fault("FORBIDDEN", "only a trusted issuer can amend an instruction")
	}
	if err := e.requireNoRecovery(ctx); err != nil {
		return nil, false, err
	}
	var view *AmendmentView
	var created bool
	err = e.inTx(ctx, "", func(tx pgx.Tx) error {
		var mandateID, sourceSystem, actor string
		err := tx.QueryRow(ctx, `SELECT mandate_id, source_system, actor_id FROM steelmatter.instructions WHERE tenant_id = $1 AND instruction_id = $2`,
			p.Tenant, instructionID).Scan(&mandateID, &sourceSystem, &actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return fault("NOT_FOUND", "instruction not found")
		}
		if err != nil {
			return err
		}
		ok, err := issuerEntitled(ctx, tx, p, sourceSystem)
		if err != nil {
			return err
		}
		if !ok {
			// Same response as an absent instruction: no existence oracle (F-23).
			return fault("NOT_FOUND", "instruction not found")
		}
		// Global lock order.
		m, err := lockMandate(ctx, tx, p.Tenant, mandateID, "FOR UPDATE")
		if err != nil {
			return err
		}
		var current int64
		var superseded string
		if err := tx.QueryRow(ctx, `SELECT current_revision, COALESCE(superseded_by, '') FROM steelmatter.instructions
			WHERE tenant_id = $1 AND instruction_id = $2 FOR UPDATE`, p.Tenant, instructionID).Scan(&current, &superseded); err != nil {
			return err
		}
		if superseded != "" {
			return fault("REVISION_STALE", "INSTRUCTION_SUPERSEDED: recovery replaced this instruction with %s, the one the anchor shows was dispatched", superseded)
		}
		var aStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM steelmatter.action_revisions WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 FOR UPDATE`,
			p.Tenant, instructionID, current).Scan(&aStatus); err != nil {
			return err
		}
		var permitID, iStatus string
		var amount int64
		err = tx.QueryRow(ctx, `SELECT permit_id, status, amount_minor FROM steelmatter.issuances
			WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3 AND status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED') FOR UPDATE`,
			p.Tenant, instructionID, current).Scan(&permitID, &iStatus, &amount)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := failpoint.Inject(ctx, failpoint.AmendAfterLocks); err != nil {
			return err
		}

		var recordedDigest string
		var recordedResult int64
		err = tx.QueryRow(ctx, `SELECT request_digest, result_revision FROM steelmatter.amendments
			WHERE tenant_id = $1 AND instruction_id = $2 AND issuer_principal = $3 AND amendment_id = $4`,
			p.Tenant, instructionID, p.ID, r.AmendmentID).Scan(&recordedDigest, &recordedResult)
		if err == nil {
			if recordedDigest != digest {
				return fault("AMENDMENT_ID_CONFLICT", "amendment_id was used with a different request")
			}
			view, err = loadAmendment(ctx, tx, p.Tenant, instructionID, r.AmendmentID, recordedResult)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if aStatus == "COMMITTED" || iStatus == "CONSUMED" {
			return fault("ALREADY_COMMITTED", "instruction has a committed operation")
		}
		if current != r.ExpectedRevision {
			return fault("REVISION_STALE", "expected_action_revision %d is not the current revision %d", r.ExpectedRevision, current)
		}
		if err := m.usable(); err != nil {
			return err
		}
		resolved, err := resolveActor(ctx, tx, p.Tenant, sourceSystem, mandateID, m)
		if err != nil {
			return err
		}
		if resolved != actor {
			return fault("ACTOR_ASSIGNMENT_INVALID", "assignment no longer selects this instruction's actor")
		}
		next := current + 1
		if next > contract.MaxSafeInteger {
			return fault("ARITHMETIC_OVERFLOW", "revision overflow")
		}
		var released int64
		var invalidated []string
		if iStatus == "UNSIGNED" || iStatus == "ACTIVE" {
			released = amount
			invalidated = []string{permitID}
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.issuances SET status = 'RELEASED', release_reason = 'AMENDED', released_at = clock_timestamp()
				WHERE tenant_id = $1 AND permit_id = $2`, p.Tenant, permitID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE steelmatter.mandates SET reserved_minor = reserved_minor - $3, available_minor = available_minor + $3
				WHERE tenant_id = $1 AND mandate_id = $2`, p.Tenant, mandateID, released); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.action_revisions SET status = 'SUPERSEDED' WHERE tenant_id = $1 AND instruction_id = $2 AND action_revision = $3`,
			p.Tenant, instructionID, current); err != nil {
			return err
		}
		canonical, adigest, a, err := buildAction(ctx, tx, p.Tenant, instructionID, next, actor, m, norm)
		if err != nil {
			return err
		}
		if err := insertRevision(ctx, tx, a, canonical, adigest); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE steelmatter.instructions SET current_revision = $3 WHERE tenant_id = $1 AND instruction_id = $2`,
			p.Tenant, instructionID, next); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.amendments (tenant_id, instruction_id, issuer_principal, amendment_id, request_digest,
			expected_revision, result_revision, released_minor, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp())`,
			p.Tenant, instructionID, p.ID, r.AmendmentID, digest, current, next, released); err != nil {
			return err
		}
		created = true
		if invalidated == nil {
			invalidated = []string{}
		}
		view = &AmendmentView{AmendmentID: r.AmendmentID, InstructionID: instructionID, PreviousRevision: current, ActionRevision: next,
			Action: canonical, ActionDigest: adigest, InvalidatedPermitIDs: invalidated, ReleasedMinor: released}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return view, created, nil
}

func loadAmendment(ctx context.Context, tx pgx.Tx, tenant, instructionID, amendmentID string, result int64) (*AmendmentView, error) {
	v := AmendmentView{AmendmentID: amendmentID, InstructionID: instructionID, ActionRevision: result, PreviousRevision: result - 1, InvalidatedPermitIDs: []string{}}
	if err := tx.QueryRow(ctx, `SELECT a.action_canonical, a.action_digest, m.released_minor
		FROM steelmatter.action_revisions a JOIN steelmatter.amendments m
		  ON m.tenant_id = a.tenant_id AND m.instruction_id = a.instruction_id AND m.result_revision = a.action_revision
		WHERE a.tenant_id = $1 AND a.instruction_id = $2 AND a.action_revision = $3 AND m.amendment_id = $4`,
		tenant, instructionID, result, amendmentID).Scan(&v.Action, &v.ActionDigest, &v.ReleasedMinor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT permit_id FROM steelmatter.issuances WHERE tenant_id = $1 AND instruction_id = $2
		AND action_revision = $3 AND release_reason = 'AMENDED' ORDER BY permit_id`, tenant, instructionID, result-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		v.InvalidatedPermitIDs = append(v.InvalidatedPermitIDs, id)
	}
	return &v, rows.Err()
}
