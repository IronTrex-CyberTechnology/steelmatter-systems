package procurement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountingFixture is a labelled synthetic invoice-reference and
// payment-request pair (T29, fixtures/accounting). It is downstream
// accounts-payable data for the synthetic destination: Steelmatter authorises
// the purchasing commitment, and money movement belongs to a separate
// integration. Nothing here calls a bank or changes the allowance ledger.
type AccountingFixture struct {
	Schema    string `json:"schema"`
	Synthetic bool   `json:"synthetic"`
	Label     string `json:"label"`
	Case      string `json:"case"`
	Invoice   struct {
		InvoiceRefID string `json:"invoice_ref_id"`
		Currency     string `json:"currency"`
		AmountMinor  int64  `json:"amount_minor"`
	} `json:"invoice_reference"`
	Payment struct {
		PaymentRequestID string `json:"payment_request_id"`
		Currency         string `json:"currency"`
		AmountMinor      int64  `json:"amount_minor"`
	} `json:"payment_request"`
}

// LoadAccountingFixture reads and checks a fixture file. Unknown fields are
// rejected and the fixture must be labelled synthetic.
func LoadAccountingFixture(path string) (*AccountingFixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f AccountingFixture
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("accounting fixture %s: %w", path, err)
	}
	if f.Schema != "steelmatter-accounting-fixture/1" || !f.Synthetic || !strings.HasPrefix(f.Label, "SYNTHETIC ") {
		return nil, fmt.Errorf("accounting fixture %s: must be schema steelmatter-accounting-fixture/1 and labelled synthetic", path)
	}
	return &f, nil
}

// AccountingLink is the persisted chain for one committed operation.
type AccountingLink struct {
	OperationID      string
	InstructionID    string
	ActionDigest     string
	OrderID          string
	ObligationID     string
	ObligationAmount int64
	InvoiceRefID     string
	PaymentRequestID string
}

// LinkAccountingFixtures records the fixture's invoice reference and payment
// request against the obligation of a committed operation, in one
// transaction, as trusted administrator fixture data. IDs are suffixed with
// suffix so repeated demo runs stay distinct. The database refuses links
// whose order, obligation or amount differ from the committed obligation, and
// a second invoice or payment request for the same obligation.
func LinkAccountingFixtures(ctx context.Context, admin *pgxpool.Pool, tenant, operationID, suffix string, f *AccountingFixture) (*AccountingLink, error) {
	tx, err := admin.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	l := AccountingLink{OperationID: operationID}
	if err := tx.QueryRow(ctx, `SELECT o.instruction_id, o.action_digest, o.target_order_id, b.obligation_id, b.amount_minor
		FROM steelmatter.operations o JOIN steelmatter.obligations b ON b.tenant_id = o.tenant_id AND b.obligation_id = o.obligation_id
		WHERE o.tenant_id = $1 AND o.operation_id = $2`, tenant, operationID).
		Scan(&l.InstructionID, &l.ActionDigest, &l.OrderID, &l.ObligationID, &l.ObligationAmount); err != nil {
		return nil, fmt.Errorf("committed operation %s: %w", operationID, err)
	}
	l.InvoiceRefID = f.Invoice.InvoiceRefID + suffix
	l.PaymentRequestID = f.Payment.PaymentRequestID + suffix
	if _, err := tx.Exec(ctx, `INSERT INTO target.invoice_references (tenant_id, invoice_ref_id, obligation_id, order_id, currency, amount_minor, synthetic, label)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenant, l.InvoiceRefID, l.ObligationID, l.OrderID, f.Invoice.Currency, f.Invoice.AmountMinor, f.Synthetic, f.Label); err != nil {
		return nil, fmt.Errorf("invoice reference: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO target.payment_requests (tenant_id, payment_request_id, invoice_ref_id, obligation_id, currency, amount_minor, synthetic, label)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenant, l.PaymentRequestID, l.InvoiceRefID, l.ObligationID, f.Payment.Currency, f.Payment.AmountMinor, f.Synthetic, f.Label); err != nil {
		return nil, fmt.Errorf("payment request: %w", err)
	}
	return &l, tx.Commit(ctx)
}
