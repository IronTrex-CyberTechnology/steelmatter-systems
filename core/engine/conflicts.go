package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/auth"
)

// Operator resolution of a source-record conflict (Aakhil, 30 Oct 2026: an
// instruction that was dispatched, spent or uncertain stops for operator
// review and its held or committed amount is preserved).
//
// Recovery supersedes an instruction made after a restore only if the anchor
// proves it never spent (releaseUnspent). If it did spend, reconciliation
// stops with REBUILD_REFUSED CONFLICT_SPENT, writes stay off, and nothing is
// released or sent. The operator reviews the two orders at the destination
// and, once the business has decided what to do with the duplicate, calls
// AdoptAnchoredOperation. It records the anchored operation again (its amount
// held, then settled by lookup like any rebuilt operation), hands the source
// key to the anchored instruction, keeps every row, order and amount of the
// spent one, and records a SOURCE_CONFLICT_ADOPTED finding for each operation
// under both instructions. The resume gate stays closed (SOURCE_RECORDS_UNIQUE)
// until every operation of a source record with orders under more than one
// instruction has that finding. Neither step creates or cancels an order and
// neither releases any amount.

// sourceConflictFinding is the text every finding recorded by
// AdoptAnchoredOperation starts with.
const sourceConflictFinding = "SOURCE_CONFLICT_ADOPTED"

// ConflictResolution is the result of AdoptAnchoredOperation.
type ConflictResolution struct {
	OpRef                 string   `json:"op_ref"`
	InstructionID         string   `json:"instruction_id"`
	SupersededInstruction string   `json:"superseded_instruction,omitempty"`
	Operations            []string `json:"operations"`
	Rebuilt               bool     `json:"rebuilt"`
	FindingSeqs           []int64  `json:"finding_seqs"`
}

// AdoptAnchoredOperation is the operator's resolution of CONFLICT_SPENT for
// one anchored operation. It runs only while writes are off. It is safe to
// repeat: a rebuilt operation is not rebuilt again and an operation that
// already has the finding gets no second one.
func (e *Engine) AdoptAnchoredOperation(ctx context.Context, p auth.Principal, opRef, evidenceRef, note string) (*ConflictResolution, error) {
	if p.Kind != auth.Operator {
		return nil, fault("FORBIDDEN", "only an operator resolves a source-record conflict")
	}
	if err := e.needAnchor(); err != nil {
		return nil, err
	}
	if err := e.ServesTenant(p.Tenant); err != nil {
		return nil, err
	}
	if evidenceRef == "" || note == "" {
		return nil, fault("SCHEMA_VIOLATION", "an evidence reference and a note are required")
	}
	unlock, err := e.recoveryLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return nil, dependency("recovery anchor", err)
	}
	if ws.Enabled {
		return nil, fault("FORBIDDEN", "a conflict is resolved only while writes are off (during recovery)")
	}
	ir, err := e.priorIntent(ctx, p.Tenant, opRef)
	if err != nil {
		return nil, dependency("recovery anchor", err)
	}
	if ir == nil {
		return nil, fault("NOT_FOUND", "the recovery anchor has no dispatch intent for this operation")
	}
	if ir.SourceSystem == "" || ir.SourceRecordID == "" {
		return nil, fault("REVISION_STALE", "REBUILD_REFUSED SOURCE_UNKNOWN: the anchor's intent for %s predates the source fields, so it cannot be tied to a source record", opRef)
	}
	res := &ConflictResolution{OpRef: opRef, InstructionID: ir.InstructionID, Operations: []string{opRef}, FindingSeqs: []int64{}}
	var rebuilt bool
	var other string
	err = e.Pool.QueryRow(ctx, `SELECT instruction_id FROM steelmatter.instructions
		WHERE tenant_id = $1 AND source_system = $2 AND source_record_id = $3 AND superseded_by IS NULL AND instruction_id <> $4`,
		p.Tenant, ir.SourceSystem, ir.SourceRecordID, ir.InstructionID).Scan(&other)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		other = ""
	case err != nil:
		return nil, unavailable(err)
	}
	var exists bool
	if err := e.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM steelmatter.dispatches WHERE tenant_id = $1 AND op_ref = $2)`, p.Tenant, opRef).Scan(&exists); err != nil {
		return nil, unavailable(err)
	}
	if !exists {
		if other == "" {
			return nil, fault("REVISION_STALE", "no instruction holds the source record %s/%s, so there is no conflict to adopt over; run reconcile", ir.SourceSystem, ir.SourceRecordID)
		}
		if rebuilt, err = e.recreateDispatchOpt(ctx, *ir, true); err != nil {
			return nil, err
		}
		res.Rebuilt = rebuilt
	}
	// Every operation under the source record, under any instruction.
	rows, err := e.Pool.Query(ctx, `SELECT d.op_ref, d.instruction_id, d.status FROM steelmatter.dispatches d
		JOIN steelmatter.instructions i ON i.tenant_id = d.tenant_id AND i.instruction_id = d.instruction_id
		WHERE d.tenant_id = $1 AND i.source_system = $2 AND i.source_record_id = $3 ORDER BY d.op_ref`, p.Tenant, ir.SourceSystem, ir.SourceRecordID)
	if err != nil {
		return nil, unavailable(err)
	}
	type opInfo struct{ op, instruction, status string }
	var ops []opInfo
	for rows.Next() {
		var o opInfo
		if err := rows.Scan(&o.op, &o.instruction, &o.status); err != nil {
			rows.Close()
			return nil, err
		}
		ops = append(ops, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	if len(ops) < 2 {
		return res, fault("REVISION_STALE", "source record %s/%s has operations under only one instruction; there is no conflict to adopt over", ir.SourceSystem, ir.SourceRecordID)
	}
	res.Operations = res.Operations[:0]
	for _, o := range ops {
		res.Operations = append(res.Operations, o.op)
		if o.instruction != ir.InstructionID {
			res.SupersededInstruction = o.instruction
		}
		done, err := e.hasSourceConflictFinding(ctx, p.Tenant, o.op)
		if err != nil {
			return nil, err
		}
		if done {
			continue
		}
		seq, err := e.Anchor.Append(ctx, anchor.OperatorFinding, p.Tenant, o.op, FindingRecord{
			Finding: fmt.Sprintf("%s: source record %s/%s has operations under more than one instruction; %s", sourceConflictFinding,
				ir.SourceSystem, ir.SourceRecordID, note),
			EvidenceRef: evidenceRef, RecordedBy: p.ID, Status: o.status}, 0)
		if err != nil {
			return res, dependency("recovery anchor", err)
		}
		res.FindingSeqs = append(res.FindingSeqs, seq)
	}
	if !rebuilt && len(res.FindingSeqs) == 0 {
		return res, nil // a repeat changed nothing
	}
	if err := e.inTx(ctx, opRef, func(tx pgx.Tx) error {
		return recoveryEvent(ctx, tx, *ir, "instruction.source_conflict_adopted", map[string]any{
			"source_system": ir.SourceSystem, "source_record_id": ir.SourceRecordID, "instruction_id": ir.InstructionID,
			"superseded_instruction": res.SupersededInstruction, "operations": res.Operations, "evidence_ref": evidenceRef, "operator": p.ID})
	}); err != nil {
		return res, err
	}
	return res, nil
}

func (e *Engine) hasSourceConflictFinding(ctx context.Context, tenant, opRef string) (bool, error) {
	recs, err := e.Anchor.RecordsFor(ctx, tenant, opRef)
	if err != nil {
		return false, dependency("recovery anchor", err)
	}
	for _, r := range recs {
		if r.Type != anchor.OperatorFinding {
			continue
		}
		var f FindingRecord
		if json.Unmarshal(r.Payload, &f) == nil && strings.HasPrefix(f.Finding, sourceConflictFinding) {
			return true, nil
		}
	}
	return false, nil
}

// sourceConflictProblems lists operations of a source record that has
// operations under more than one instruction and lacks the operator's
// SOURCE_CONFLICT_ADOPTED finding: two orders for one ERP record that nobody
// has reviewed (resume gate check SOURCE_RECORDS_UNIQUE).
func (e *Engine) sourceConflictProblems(ctx context.Context) ([]string, error) {
	rows, err := e.Pool.Query(ctx, `SELECT d.tenant_id, d.op_ref, i.source_system, i.source_record_id
		FROM steelmatter.dispatches d JOIN steelmatter.instructions i ON i.tenant_id = d.tenant_id AND i.instruction_id = d.instruction_id
		WHERE (i.tenant_id, i.source_system, i.source_record_id) IN (
			SELECT i2.tenant_id, i2.source_system, i2.source_record_id FROM steelmatter.instructions i2
			JOIN steelmatter.dispatches d2 ON d2.tenant_id = i2.tenant_id AND d2.instruction_id = i2.instruction_id
			GROUP BY 1, 2, 3 HAVING count(DISTINCT i2.instruction_id) > 1)
		ORDER BY d.op_ref`)
	if err != nil {
		return nil, unavailable(err)
	}
	type row struct{ tenant, op, system, record string }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenant, &r.op, &r.system, &r.record); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	var out []string
	for _, r := range list {
		done, err := e.hasSourceConflictFinding(ctx, r.tenant, r.op)
		if err != nil {
			return nil, err
		}
		if !done {
			out = append(out, fmt.Sprintf("%s: source record %s/%s has operations under more than one instruction; an operator must review the duplicate and "+
				"record the resolution (POST /v1/recovery/conflicts/{op_ref}/adopt)", r.op, r.system, r.record))
		}
	}
	sort.Strings(out)
	return out, nil
}
