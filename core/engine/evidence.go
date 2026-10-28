package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/failpoint"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/observer"
)

// EvidenceRecords holds the signed records exactly as issued.
type EvidenceRecords struct {
	MandateJWS     string `json:"mandate_jws,omitempty"`
	PermitJWS      string `json:"permit_jws,omitempty"`
	ExecutionJWS   string `json:"execution_jws,omitempty"`
	ObservationJWS string `json:"observation_jws,omitempty"`
}

// EvidenceLinks are the service's linkage claims; verifiers recompute them
// from the signed payloads rather than trusting them.
type EvidenceLinks struct {
	InstructionID   string `json:"instruction_id"`
	ActionDigest    string `json:"action_digest"`
	MandateDigest   string `json:"mandate_digest,omitempty"`
	PermitDigest    string `json:"permit_digest,omitempty"`
	ExecutionDigest string `json:"execution_digest,omitempty"`
	ObligationID    string `json:"obligation_id,omitempty"`
}

// EvidenceBundle is the export served by GET /v1/operations/{id}/evidence.
// It contains no secrets and no trust anchors: verifiers must supply their own.
type EvidenceBundle struct {
	OperationID string          `json:"operation_id"`
	Evidence    string          `json:"evidence"`
	Records     EvidenceRecords `json:"records"`
	Action      json.RawMessage `json:"action"`
	Links       EvidenceLinks   `json:"links"`
	Incomplete  []string        `json:"incomplete"`
}

// ReconcileResult is returned by reconcile.
type ReconcileResult struct {
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Observation string `json:"observation"`
}

type opScope struct {
	instructionID, actor, issuer, owner, mandateID string
	committed                                      bool
}

// scope loads who may read an operation; unauthorised callers get NOT_FOUND.
func (e *Engine) scope(ctx context.Context, p auth.Principal, operationID string, kinds ...string) (*opScope, error) {
	var s opScope
	var opID *string
	var entitled bool
	err := e.Pool.QueryRow(ctx, `SELECT i.instruction_id, n.actor_id, n.issuer_principal, m.owner_principal, m.mandate_id, o.operation_id,
		EXISTS (SELECT 1 FROM steelmatter.issuer_entitlements x WHERE x.tenant_id = n.tenant_id AND x.issuer_principal = $3 AND x.source_system = n.source_system)
		FROM steelmatter.issuances i
		JOIN steelmatter.instructions n ON n.tenant_id = i.tenant_id AND n.instruction_id = i.instruction_id
		JOIN steelmatter.mandates m ON m.tenant_id = i.tenant_id AND m.mandate_id = i.mandate_id
		LEFT JOIN steelmatter.evidence_operations o ON o.tenant_id = i.tenant_id AND o.operation_id = i.operation_id
		WHERE i.tenant_id = $1 AND i.operation_id = $2`, p.Tenant, operationID, p.ID).
		Scan(&s.instructionID, &s.actor, &s.issuer, &s.owner, &s.mandateID, &opID, &entitled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fault("NOT_FOUND", "operation not found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	s.committed = opID != nil
	allowed := false
	for _, k := range kinds {
		switch {
		case k == auth.Observer && p.Kind == auth.Observer, k == auth.Operator && p.Kind == auth.Operator:
			allowed = true
		case k == auth.Actor && p.Kind == auth.Actor && p.ID == s.actor,
			// Issuers read by current entitlement to the source workflow, the
			// same rule that governs amendment (F-23).
			k == auth.Issuer && p.Kind == auth.Issuer && entitled,
			k == auth.Owner && p.Kind == auth.Owner && p.ID == s.owner:
			allowed = true
		}
	}
	if !allowed {
		return nil, fault("NOT_FOUND", "operation not found")
	}
	return &s, nil
}

var readers = []string{auth.Actor, auth.Issuer, auth.Owner, auth.Observer, auth.Operator}

// SignExecution signs a committed operation's stored execution payload if it
// is not yet signed. Signing happens outside the commit transaction, so
// evidence may be PENDING briefly after commit; the stored bytes never change.
func (e *Engine) SignExecution(ctx context.Context, tenant, operationID string) error {
	var canonical []byte
	var external bool
	err := e.Pool.QueryRow(ctx, `SELECT execution_canonical, external_destination FROM steelmatter.evidence_operations
		WHERE tenant_id = $1 AND operation_id = $2 AND execution_jws IS NULL`, tenant, operationID).Scan(&canonical, &external)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	signer, err := e.signer("SM-EXECUTION+JWS")
	if err != nil {
		return err // evidence stays PENDING until a current key is configured
	}
	if err := failpoint.Inject(ctx, failpoint.EvidenceBeforeSign); err != nil {
		return err
	}
	jws, err := contract.Sign(contract.KindExecution, canonical, signer)
	if err != nil {
		return err
	}
	table := "steelmatter.operations"
	if external {
		table = "steelmatter.r1_executions"
	}
	_, err = e.Pool.Exec(ctx, `UPDATE `+table+` SET execution_jws = $3
		WHERE tenant_id = $1 AND operation_id = $2 AND execution_jws IS NULL`, tenant, operationID, jws)
	return err
}

// SignPendingExecutions signs unsigned execution records that are not
// backed off. A record that fails is backed off and the rest are still
// signed (SEC-DEF-002). Without a current execution key nothing is
// attempted. Returns the number signed.
func (e *Engine) SignPendingExecutions(ctx context.Context) (int, error) {
	buildErr := e.buildPendingR1Evidence(ctx)
	ops, err := e.listOps(ctx, `SELECT o.tenant_id, o.operation_id FROM steelmatter.evidence_operations o
		WHERE o.execution_jws IS NULL AND `+notBackedOff(workerEvidence, "o.tenant_id", "o.operation_id")+`
		ORDER BY o.committed_at LIMIT 100`)
	if err != nil || len(ops) == 0 {
		return 0, errors.Join(buildErr, err)
	}
	if _, err := e.signer("SM-EXECUTION+JWS"); err != nil {
		return 0, errors.Join(buildErr, err)
	}
	n := 0
	err = runIsolated(ctx, e, workerEvidence, ops, opKey, func(o [2]string) error {
		if err := e.SignExecution(failpoint.WithKey(ctx, o[1]), o[0], o[1]); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, errors.Join(buildErr, err)
}

// ObserveUnobserved records a first observation for committed operations
// that have none and are not backed off. A failing operation is backed off
// and the rest are still observed (SEC-DEF-003). Without a current observer
// key nothing is attempted. Returns the number observed.
func (e *Engine) ObserveUnobserved(ctx context.Context) (int, error) {
	if e.Observer == nil {
		return 0, nil
	}
	ops, err := e.listOps(ctx, `SELECT o.tenant_id, o.operation_id FROM steelmatter.evidence_operations o
		WHERE NOT EXISTS (SELECT 1 FROM steelmatter.evidence_observations b WHERE b.tenant_id = o.tenant_id AND b.operation_id = o.operation_id)
		AND `+notBackedOff(workerObserver, "o.tenant_id", "o.operation_id")+`
		ORDER BY o.committed_at LIMIT 100`)
	if err != nil || len(ops) == 0 {
		return 0, err
	}
	if err := e.observerKeyCurrent(); err != nil {
		return 0, err
	}
	n := 0
	err = runIsolated(ctx, e, workerObserver, ops, opKey, func(o [2]string) error {
		r, err := e.observe(failpoint.WithKey(ctx, o[1]), o[0], o[1], true)
		if err != nil {
			return err
		}
		if r != nil {
			n++
		}
		return nil
	})
	return n, err
}

func opKey(o [2]string) (string, string) { return o[0], o[1] }

func (e *Engine) listOps(ctx context.Context, sql string) ([][2]string, error) {
	rows, err := e.Pool.Query(ctx, sql)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var t, id string
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		out = append(out, [2]string{t, id})
	}
	return out, rows.Err()
}

// observe runs a read-only readback and stores the observer-signed result.
// Storing locks the operation row, so a worker's first observation and a
// concurrent Reconcile are serialised. With firstOnly (the worker), the
// readback is discarded and nil returned if the operation was observed
// meanwhile, so it never gets two first observations.
func (e *Engine) observe(ctx context.Context, tenant, operationID string, firstOnly bool) (*observer.Result, error) {
	if err := e.observerKeyCurrent(); err != nil {
		return nil, err
	}
	if err := failpoint.Inject(ctx, failpoint.ObserverBeforeReadback); err != nil {
		return nil, err
	}
	var external bool
	if err := e.Pool.QueryRow(ctx, `SELECT external_destination FROM steelmatter.evidence_operations WHERE tenant_id=$1 AND operation_id=$2`, tenant, operationID).Scan(&external); err != nil {
		return nil, err
	}
	var r *observer.Result
	var err error
	operationTable, observationTable := "steelmatter.operations", "steelmatter.observations"
	if external {
		operationTable, observationTable = "steelmatter.r1_executions", "steelmatter.r1_observations"
		r, err = e.Observer.ObserveDestination(ctx, tenant, operationID, e.Connector)
	} else {
		r, err = e.Observer.Observe(ctx, tenant, operationID)
	}
	if err != nil {
		return nil, err
	}
	if err := failpoint.Inject(ctx, failpoint.ObserverBeforeStore); err != nil {
		return nil, err
	}
	stored := false
	err = e.inTx(ctx, "", func(tx pgx.Tx) error {
		stored = false
		if _, err := tx.Exec(ctx, `SELECT 1 FROM `+operationTable+` WHERE tenant_id = $1 AND operation_id = $2 FOR NO KEY UPDATE`,
			tenant, operationID); err != nil {
			return err
		}
		if firstOnly {
			var observed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM steelmatter.evidence_observations WHERE tenant_id = $1 AND operation_id = $2)`,
				tenant, operationID).Scan(&observed); err != nil {
				return err
			}
			if observed {
				return nil
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+observationTable+` (tenant_id, observation_id, operation_id, status, expected_action_digest,
			observed_action_digest, observation_canonical, observation_digest, observation_jws, observed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			tenant, r.ObservationID, operationID, r.Status, r.ExpectedActionDigest, r.ObservedActionDigest, r.Canonical, r.Digest, r.JWS, r.ObservedAt); err != nil {
			return err
		}
		stored = true
		return nil
	})
	if err != nil || !stored {
		return nil, err
	}
	return r, nil
}

// observerKeyCurrent refuses when the observer's signing key is retired or
// untrusted. This affects every operation, so workers check it up front.
func (e *Engine) observerKeyCurrent() error {
	if k, ok := e.Trust.Lookup(e.Observer.Signer.Kid); !ok || k.RetiredAt(e.Now()) {
		return fault("SERVICE_UNAVAILABLE", "no current signing key for SM-OBSERVATION+JWS")
	}
	return nil
}

// Reconcile performs a readback now (200 RESOLVED). It is read-only against
// the destination: it cannot invent success or create a replacement order.
// An operation that never committed has nothing to read back.
func (e *Engine) Reconcile(ctx context.Context, p auth.Principal, operationID string) (*ReconcileResult, error) {
	s, err := e.scope(ctx, p, operationID, auth.Observer, auth.Operator)
	if err != nil {
		return nil, err
	}
	res := &ReconcileResult{OperationID: operationID, State: "RESOLVED", Observation: "NOT_OBSERVED"}
	if !s.committed {
		return res, nil
	}
	if e.Observer == nil {
		return nil, fault("SERVICE_UNAVAILABLE", "observer is not configured")
	}
	r, err := e.observe(ctx, p.Tenant, operationID, false)
	if err != nil {
		return nil, unavailable(err)
	}
	res.Observation = r.Status
	return res, nil
}

// Evidence assembles the signed records and linkage for an authorised reader.
func (e *Engine) Evidence(ctx context.Context, p auth.Principal, operationID string) (*EvidenceBundle, error) {
	s, err := e.scope(ctx, p, operationID, readers...)
	if err != nil {
		return nil, err
	}
	return e.loadEvidence(ctx, p.Tenant, operationID, s.instructionID)
}

// storedEvidence holds what the service recorded, against which the signed
// records are checked.
type storedEvidence struct {
	operationID, mandateDigest, actionDigest  string
	permitDigest, executionDigest, obligation *string
	actionCanonical                           []byte
}

func (e *Engine) loadEvidence(ctx context.Context, tenant, operationID, instructionID string) (*EvidenceBundle, error) {
	b := &EvidenceBundle{OperationID: operationID, Incomplete: []string{}}
	b.Links.InstructionID = instructionID
	st := storedEvidence{operationID: operationID}
	var mandateJWS, permitJWS, execJWS, obsJWS *string
	err := e.Pool.QueryRow(ctx, `SELECT m.mandate_jws, m.mandate_digest, i.permit_jws, i.permit_digest, a.action_canonical, a.action_digest,
		o.execution_jws, o.execution_digest, o.obligation_id,
		(SELECT observation_jws FROM steelmatter.evidence_observations b WHERE b.tenant_id = i.tenant_id AND b.operation_id = i.operation_id ORDER BY recorded_at DESC LIMIT 1)
		FROM steelmatter.issuances i
		JOIN steelmatter.mandates m ON m.tenant_id = i.tenant_id AND m.mandate_id = i.mandate_id
		JOIN steelmatter.action_revisions a ON a.tenant_id = i.tenant_id AND a.instruction_id = i.instruction_id AND a.action_revision = i.action_revision
		LEFT JOIN steelmatter.evidence_operations o ON o.tenant_id = i.tenant_id AND o.operation_id = i.operation_id
		WHERE i.tenant_id = $1 AND i.operation_id = $2`, tenant, operationID).
		Scan(&mandateJWS, &st.mandateDigest, &permitJWS, &st.permitDigest, &st.actionCanonical, &st.actionDigest, &execJWS, &st.executionDigest, &st.obligation, &obsJWS)
	if err != nil {
		return nil, unavailable(err)
	}
	b.Action = st.actionCanonical
	b.Links.MandateDigest, b.Links.ActionDigest = st.mandateDigest, st.actionDigest
	set := func(dst *string, v *string, name string) {
		if v != nil {
			*dst = *v
		} else {
			b.Incomplete = append(b.Incomplete, name)
		}
	}
	set(&b.Records.MandateJWS, mandateJWS, "mandate_jws")
	set(&b.Records.PermitJWS, permitJWS, "permit_jws")
	set(&b.Records.ExecutionJWS, execJWS, "execution_jws")
	set(&b.Records.ObservationJWS, obsJWS, "observation_jws")
	if permitJWS != nil && st.permitDigest != nil {
		b.Links.PermitDigest = *st.permitDigest
	}
	if st.executionDigest != nil {
		b.Links.ExecutionDigest = *st.executionDigest
	}
	if st.obligation != nil {
		b.Links.ObligationID = *st.obligation
	}
	b.Evidence = e.evidenceStatus(b.Records, st)
	return b, nil
}

// evidenceStatus: INVALID if any present record fails verification under the
// service trust store, or if a verified record does not match what the
// service stored for this operation or does not link to the other records
// (F-10); AVAILABLE once the execution record is signed; otherwise PENDING.
func (e *Engine) evidenceStatus(r EvidenceRecords, st storedEvidence) string {
	verify := func(jws string, k contract.Kind) (*contract.Verified, bool) {
		if jws == "" {
			return nil, true
		}
		v, err := contract.Verify(jws, k, e.Trust)
		return v, err == nil
	}
	mv, ok1 := verify(r.MandateJWS, contract.KindMandate)
	pv, ok2 := verify(r.PermitJWS, contract.KindPermit)
	ev, ok3 := verify(r.ExecutionJWS, contract.KindExecution)
	ov, ok4 := verify(r.ObservationJWS, contract.KindObservation)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return "INVALID"
	}
	// A record whose own time is at or after its key's retirement is not
	// trustworthy historical evidence.
	for _, v := range []*contract.Verified{mv, pv, ev, ov} {
		if v != nil && !signedWithinValidity(v) {
			return "INVALID"
		}
	}
	actionDigest := contract.Digest(contract.KindAction, st.actionCanonical)
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	consistent := actionDigest == st.actionDigest
	if mv != nil {
		consistent = consistent && mv.Digest == st.mandateDigest
	}
	if pv != nil {
		pp := pv.Value.(*contract.Permit)
		consistent = consistent && pv.Digest == deref(st.permitDigest) && pp.OperationID == st.operationID &&
			pp.ActionDigest == actionDigest && pp.MandateDigest == st.mandateDigest
	}
	if ev != nil {
		ep := ev.Value.(*contract.Execution)
		consistent = consistent && ev.Digest == deref(st.executionDigest) && ep.OperationID == st.operationID &&
			ep.ActionDigest == actionDigest && ep.ObligationID == deref(st.obligation) && (pv == nil || ep.PermitDigest == pv.Digest)
	}
	if ov != nil {
		op := ov.Value.(*contract.Observation)
		consistent = consistent && op.OperationID == st.operationID && op.ExpectedActionDigest == actionDigest &&
			op.ExecutionDigest == deref(st.executionDigest)
	}
	if ev != nil && ov != nil && ev.Value.(*contract.Execution).SchemaVersion == contract.ExternalEvidenceVersion {
		ex, ob := ev.Value.(*contract.Execution), ov.Value.(*contract.Observation)
		consistent = consistent && ob.SchemaVersion == contract.ExternalEvidenceVersion && ob.ExpectedCompanyRef == ex.CompanyRef && ob.ExpectedPermitDigest == ex.PermitDigest
	}
	if !consistent {
		return "INVALID"
	}
	// Cross-record timestamps must be possible (ADR-003): a validly signed
	// chain whose records contradict each other in time is not evidence.
	var mp *contract.Mandate
	var pp *contract.Permit
	var ep *contract.Execution
	var op *contract.Observation
	if mv != nil {
		mp = mv.Value.(*contract.Mandate)
	}
	if pv != nil {
		pp = pv.Value.(*contract.Permit)
	}
	if ev != nil {
		ep = ev.Value.(*contract.Execution)
	}
	if ov != nil {
		op = ov.Value.(*contract.Observation)
	}
	if !contract.TemporalConsistent(contract.TemporalLinkage(mp, pp, ep, op)) {
		return "INVALID"
	}
	if r.ExecutionJWS == "" {
		return "PENDING"
	}
	return "AVAILABLE"
}

// signedWithinValidity checks the record's own timestamp against its signing
// key's retirement (NotAfter).
func signedWithinValidity(v *contract.Verified) bool {
	if v.SignedKey.NotAfter == nil {
		return true
	}
	var at string
	switch p := v.Value.(type) {
	case *contract.Mandate:
		at = p.IssuedAt
	case *contract.Permit:
		at = p.IssuedAt
	case *contract.Execution:
		at = p.CommittedAt
	case *contract.Observation:
		at = p.ObservedAt
	}
	t, err := contract.ParseTimestamp(at)
	return err == nil && t.Before(*v.SignedKey.NotAfter)
}
