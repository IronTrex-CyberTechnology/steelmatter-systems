// Package observer performs read-only readback of the synthetic destination
// and produces signed observations (T24, T25). It connects with the observer
// database role, which can only SELECT; it never writes.
//
// Projection (docs/contracts/I02-contract.md §8, pinned in docs/evidence.md):
// the observed action is the expected action with every material field taken
// from the destination readback: supplier_id, supplier_revision, delivery_id,
// currency, lines and amount_minor. Identity fields the destination does not
// hold (schema_version, crypto_profile, tenant_id, instruction_id,
// action_revision, actor_id, audience, operation) come from the stored
// operation. A missing order, line set or field yields INSUFFICIENT_EVIDENCE
// with no observed digest; MATCH requires equal digests.
package observer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
)

// Observer reads with the observer role and signs with the observation key.
type Observer struct {
	Pool       *pgxpool.Pool // observer role: SELECT only
	Signer     contract.Signer
	ObserverID string
}

// Result is a signed observation, ready to be stored by the service.
type Result struct {
	ObservationID        string
	OperationID          string
	Status               string
	ExpectedActionDigest string
	ObservedActionDigest *string
	Canonical            []byte
	Digest               string
	JWS                  string
	ObservedAt           time.Time
}

// ErrUnknownOperation means no committed operation exists for the ID.
var ErrUnknownOperation = errors.New("observer: unknown operation")

// Observe reads the destination for one committed operation and returns a
// signed observation. It performs no writes.
func (o *Observer) Observe(ctx context.Context, tenant, operationID string) (*Result, error) {
	tx, err := o.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var instructionID, orderID, expectedDigest, executionDigest string
	var actionRev int64
	var expectedCanonical []byte
	err = tx.QueryRow(ctx, `SELECT o.instruction_id, o.action_revision, o.target_order_id, o.action_digest, o.execution_digest, a.action_canonical
		FROM steelmatter.operations o JOIN steelmatter.action_revisions a
		  ON a.tenant_id = o.tenant_id AND a.instruction_id = o.instruction_id AND a.action_revision = o.action_revision
		WHERE o.tenant_id = $1 AND o.operation_id = $2`, tenant, operationID).
		Scan(&instructionID, &actionRev, &orderID, &expectedDigest, &executionDigest, &expectedCanonical)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnknownOperation
	}
	if err != nil {
		return nil, err
	}
	var expected map[string]any
	dec := json.NewDecoder(bytes.NewReader(expectedCanonical))
	dec.UseNumber() // keep integers exact
	if err := dec.Decode(&expected); err != nil {
		return nil, fmt.Errorf("stored action: %w", err)
	}

	observed, complete, err := readback(ctx, tx, tenant, orderID, instructionID, expected)
	if err != nil {
		return nil, err
	}
	res := &Result{ObservationID: newID("obs"), OperationID: operationID, ExpectedActionDigest: expectedDigest}
	switch {
	case !complete:
		res.Status = "INSUFFICIENT_EVIDENCE"
	default:
		raw, _ := json.Marshal(observed)
		canonical, err := contract.Canonicalize(raw)
		if err != nil {
			return nil, err
		}
		d := contract.Digest(contract.KindAction, canonical)
		res.ObservedActionDigest = &d
		res.Status = "MISMATCH"
		if d == expectedDigest {
			res.Status = "MATCH"
		}
	}
	if err := tx.QueryRow(ctx, `SELECT date_trunc('second', clock_timestamp())`).Scan(&res.ObservedAt); err != nil {
		return nil, err
	}
	payload := contract.Observation{
		SchemaVersion: contract.SchemaVersion, CryptoProfile: contract.CryptoProfile,
		ObservationID: res.ObservationID, ObserverID: o.ObserverID, OperationID: operationID, TenantID: tenant,
		InstructionID: instructionID, ExecutionDigest: executionDigest, ExpectedActionDigest: expectedDigest,
		ObservedActionDigest: res.ObservedActionDigest, TargetOrderID: orderID, Status: res.Status,
		ObservedAt: res.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	raw, _ := json.Marshal(payload)
	res.Canonical, res.Digest, err = contract.CanonicalDigest(contract.KindObservation, raw)
	if err != nil {
		return nil, err
	}
	res.JWS, err = contract.Sign(contract.KindObservation, res.Canonical, o.Signer)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// readback builds the observed action. complete is false when the order,
// its lines or any material field is missing.
func readback(ctx context.Context, tx pgx.Tx, tenant, orderID, instructionID string, expected map[string]any) (map[string]any, bool, error) {
	var supplier, delivery, currency, orderInstruction *string
	var supplierRev, amount *int64
	err := tx.QueryRow(ctx, `SELECT supplier_id, supplier_revision, delivery_id, currency, amount_minor, instruction_id
		FROM target.purchase_orders WHERE tenant_id = $1 AND order_id = $2`, tenant, orderID).
		Scan(&supplier, &supplierRev, &delivery, &currency, &amount, &orderInstruction)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if supplier == nil || supplierRev == nil || delivery == nil || currency == nil || amount == nil || orderInstruction == nil {
		return nil, false, nil
	}
	rows, err := tx.Query(ctx, `SELECT item_id, quantity, unit_price_minor FROM target.purchase_order_lines
		WHERE tenant_id = $1 AND order_id = $2 ORDER BY item_id COLLATE "C"`, tenant, orderID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var lines []map[string]any
	for rows.Next() {
		var item *string
		var qty, price *int64
		if err := rows.Scan(&item, &qty, &price); err != nil {
			return nil, false, err
		}
		if item == nil || qty == nil || price == nil {
			return nil, false, nil
		}
		lines = append(lines, map[string]any{"item_id": *item, "quantity": *qty, "unit_price_minor": *price})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(lines) == 0 {
		return nil, false, nil
	}
	observed := map[string]any{}
	for _, k := range []string{"schema_version", "crypto_profile", "tenant_id", "instruction_id", "action_revision", "actor_id", "audience", "operation"} {
		observed[k] = expected[k]
	}
	if *orderInstruction != instructionID {
		observed["instruction_id"] = *orderInstruction // destination row belongs to another instruction
	}
	observed["supplier_id"], observed["supplier_revision"] = *supplier, *supplierRev
	observed["delivery_id"], observed["currency"] = *delivery, *currency
	observed["lines"], observed["amount_minor"] = lines, *amount
	return observed, true, nil
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// CheckSeparation verifies at startup that the observer connects as a
// different database role from the executor and that this role cannot write
// any Steelmatter table (F-09).
func CheckSeparation(ctx context.Context, observerPool, executorPool *pgxpool.Pool) error {
	var obs, exec string
	if err := observerPool.QueryRow(ctx, `SELECT current_user`).Scan(&obs); err != nil {
		return err
	}
	if err := executorPool.QueryRow(ctx, `SELECT current_user`).Scan(&exec); err != nil {
		return err
	}
	if obs == exec {
		return fmt.Errorf("observer and executor share database role %q", obs)
	}
	var writable []string
	rows, err := observerPool.Query(ctx, `SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname IN ('steelmatter', 'steelmatter_meta', 'target')
		  AND (has_table_privilege(current_user, c.oid, 'INSERT') OR has_table_privilege(current_user, c.oid, 'UPDATE')
		    OR has_table_privilege(current_user, c.oid, 'DELETE') OR has_table_privilege(current_user, c.oid, 'TRUNCATE')
		    OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0
		      AND (has_column_privilege(current_user, c.oid, a.attnum, 'UPDATE') OR has_column_privilege(current_user, c.oid, a.attnum, 'INSERT'))))
		ORDER BY 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		writable = append(writable, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(writable) > 0 {
		return fmt.Errorf("observer role %q can write %v; it must be read-only", obs, writable)
	}
	return nil
}
