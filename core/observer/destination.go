package observer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
)

// DestinationReader exposes only observation, never create or fence.
// The Odoo implementation uses its separate observer credential for this call.
type DestinationReader interface {
	Observe(context.Context, connector.Action) (connector.Result, error)
}

func (o *Observer) ObserveDestination(ctx context.Context, tenant, op string, destination DestinationReader) (*Result, error) {
	if destination == nil {
		return nil, errors.New("destination observer is unavailable")
	}
	var executionDigest, company, permitDigest, orderID string
	var canonical []byte
	err := o.Pool.QueryRow(ctx, `SELECT x.execution_digest,d.company_ref,d.permit_digest,x.target_order_id,d.action_canonical
  FROM steelmatter.r1_executions x JOIN steelmatter.dispatches d ON d.tenant_id=x.tenant_id AND d.op_ref=x.operation_id
  WHERE x.tenant_id=$1 AND x.operation_id=$2`, tenant, op).Scan(&executionDigest, &company, &permitDigest, &orderID, &canonical)
	if err != nil {
		return nil, err
	}
	var expected contract.Action
	if err := json.Unmarshal(canonical, &expected); err != nil {
		return nil, err
	}
	a := connector.Action{OpRef: op, ActionRevision: expected.ActionRevision, CompanyRef: company, DeliveryRef: expected.DeliveryID,
		SupplierRef: expected.SupplierID, SupplierRevision: expected.SupplierRevision, AmountMinor: expected.AmountMinor, Currency: expected.Currency, PermitDigest: permitDigest}
	for _, l := range expected.Lines {
		a.Lines = append(a.Lines, connector.Line{ItemRef: l.ItemID, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor})
	}
	readback, err := destination.Observe(ctx, a)
	if err != nil {
		return nil, err
	}
	if readback.OpRef != "" && readback.OpRef != op {
		return nil, fmt.Errorf("observer received another operation: %s", readback.OpRef)
	}
	if readback.Outcome != connector.Match && readback.Outcome != connector.Mismatch && readback.Outcome != connector.InsufficientEvidence && readback.Outcome != connector.NotFound {
		return nil, fmt.Errorf("destination observation unavailable: %s", readback.Outcome)
	}
	res := &Result{ObservationID: newID("obs"), OperationID: op, ExpectedActionDigest: contract.Digest(contract.KindAction, canonical), Status: "INSUFFICIENT_EVIDENCE"}
	observedCompany, observedState, observedPermit := "", "", ""
	if dest := readback.Destination; dest != nil {
		observedCompany, observedState, observedPermit = dest.CompanyRef, dest.State, dest.PermitDigest
		// Identity is part of readback too. An order with another ID cannot attest
		// this execution, even if all its commercial fields happen to match.
		if dest.ID == orderID && dest.SupplierRevision > 0 && dest.SupplierRef != "" && dest.DeliveryRef != "" && dest.Currency != "" && len(dest.Lines) > 0 {
			observed := expected
			observed.SupplierID, observed.SupplierRevision, observed.DeliveryID = dest.SupplierRef, dest.SupplierRevision, dest.DeliveryRef
			observed.Currency, observed.AmountMinor = dest.Currency, dest.AmountMinor
			observed.Lines = nil
			for _, l := range dest.Lines {
				observed.Lines = append(observed.Lines, contract.Line{ItemID: l.ItemRef, Quantity: l.Quantity, UnitPriceMinor: l.UnitPriceMinor})
			}
			raw, _ := json.Marshal(observed)
			canon, err := contract.Canonicalize(raw)
			if err != nil {
				return nil, err
			}
			digest := contract.Digest(contract.KindAction, canon)
			res.ObservedActionDigest = &digest
			res.Status = "MISMATCH"
			if digest == res.ExpectedActionDigest && observedCompany == company && observedState == connector.StateConfirmed && observedPermit == permitDigest {
				res.Status = "MATCH"
			}
		}
	}
	if err := o.Pool.QueryRow(ctx, `SELECT date_trunc('second',clock_timestamp())`).Scan(&res.ObservedAt); err != nil {
		return nil, err
	}
	payload := contract.Observation{SchemaVersion: contract.ExternalEvidenceVersion, CryptoProfile: contract.CryptoProfile,
		ObservationID: res.ObservationID, ObserverID: o.ObserverID, OperationID: op, TenantID: tenant, InstructionID: expected.InstructionID,
		ExecutionDigest: executionDigest, ExpectedActionDigest: res.ExpectedActionDigest, ObservedActionDigest: res.ObservedActionDigest,
		TargetOrderID: orderID, Status: res.Status, ObservedAt: res.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"),
		ExpectedCompanyRef: company, ObservedCompanyRef: &observedCompany, ObservedState: &observedState, ExpectedPermitDigest: permitDigest, ObservedPermitDigest: &observedPermit}
	raw, _ := json.Marshal(payload)
	res.Canonical, res.Digest, err = contract.CanonicalDigest(contract.KindObservation, raw)
	if err != nil {
		return nil, err
	}
	res.JWS, err = contract.Sign(contract.KindObservation, res.Canonical, o.Signer)
	return res, err
}
