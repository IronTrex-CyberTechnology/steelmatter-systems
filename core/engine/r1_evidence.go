package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/jackc/pgx/v5"
)

// Only a confirmed, matching destination result produces execution evidence.
// Manual accounting repairs without these facts remain explicitly incomplete.
func (e *Engine) buildPendingR1Evidence(ctx context.Context) error {
	ops, err := e.listOps(ctx, `SELECT d.tenant_id,d.op_ref FROM steelmatter.dispatches d
  WHERE d.status='COMMITTED' AND d.destination_facts IS NOT NULL AND d.admitted_at IS NOT NULL
  AND NOT EXISTS(SELECT 1 FROM steelmatter.r1_executions x WHERE x.tenant_id=d.tenant_id AND x.operation_id=d.op_ref)
  AND `+notBackedOff(workerEvidence, "d.tenant_id", "d.op_ref")+` ORDER BY d.settled_at LIMIT 100`)
	if err != nil {
		return err
	}
	return runIsolated(ctx, e, workerEvidence, ops, opKey, func(k [2]string) error {
		return e.buildR1Evidence(ctx, k[0], k[1])
	})
}

func (e *Engine) buildR1Evidence(ctx context.Context, tenant, op string) error {
	return e.inTx(ctx, op, func(tx pgx.Tx) error {
		_, status, _, _, err := lockRepairOperation(ctx, tx, tenant, op)
		if err != nil {
			return err
		}
		if status != DispatchCommitted {
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM steelmatter.r1_executions WHERE tenant_id=$1 AND operation_id=$2)`, tenant, op).Scan(&exists); err != nil || exists {
			return err
		}
		var d dispatchRow
		var permitID, actionDigest string
		var canonical, facts []byte
		var admitted, settled time.Time
		err = tx.QueryRow(ctx, `SELECT company_ref,permit_id,permit_digest,action_digest,action_canonical,amount_minor,destination_facts,admitted_at,settled_at
   FROM steelmatter.dispatches WHERE tenant_id=$1 AND op_ref=$2`, tenant, op).Scan(&d.companyRef, &permitID, &d.permitDigest, &actionDigest, &canonical, &d.amount, &facts, &admitted, &settled)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(canonical, &d.action); err != nil {
			return err
		}
		var order connector.Order
		if err := json.Unmarshal(facts, &order); err != nil {
			return err
		}
		if !d.matches(&order) || order.ID == "" {
			return fmt.Errorf("cannot attest mismatched destination facts for %s", op)
		}
		obligation := newID("obl")
		payload := contract.Execution{SchemaVersion: contract.ExternalEvidenceVersion, CryptoProfile: contract.CryptoProfile,
			ExecutionID: newID("exec"), OperationID: op, TenantID: tenant, InstructionID: d.action.InstructionID, ObligationID: obligation,
			ActionDigest: actionDigest, PermitDigest: d.permitDigest, TargetOrderID: order.ID, Currency: d.action.Currency, AmountMinor: d.amount,
			Status: DispatchCommitted, AdmittedAt: ts(admitted), CommittedAt: ts(settled), CompanyRef: d.companyRef}
		raw, _ := json.Marshal(payload)
		canon, digest, err := contract.CanonicalDigest(contract.KindExecution, raw)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO steelmatter.obligations(tenant_id,obligation_id,instruction_id,operation_id,action_digest,amount_minor,created_at)
   VALUES($1,$2,$3,$4,$5,$6,$7)`, tenant, obligation, d.action.InstructionID, op, actionDigest, d.amount, settled); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO steelmatter.r1_executions(tenant_id,operation_id,instruction_id,action_revision,permit_id,action_digest,status,
   target_order_id,obligation_id,execution_canonical,execution_digest,committed_at)
   VALUES($1,$2,$3,$4,$5,$6,'COMMITTED',$7,$8,$9,$10,$11)`, tenant, op, d.action.InstructionID, d.action.ActionRevision, permitID, actionDigest, order.ID, obligation, canon, digest, settled)
		return err
	})
}
