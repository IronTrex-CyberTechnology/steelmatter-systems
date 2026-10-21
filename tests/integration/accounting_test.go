//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/examples/procurement"
)

const accountingFixture = "../../fixtures/accounting/invoice-payment-v1.json"

// T29: link the committed order, a labelled synthetic invoice-reference
// fixture and a payment-request fixture. One procurement obligation; the
// allowance is charged once (not per order, invoice and payment) and paying
// does not restore capacity.
func TestT29AccountingContinuity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.committed()
	committedLedger := ledger{Available: 40_000_000, Committed: sixLakh, Orders: 1, Obligations: 1, Operations: 1, Outbox: 1}
	requireLedger(t, e.ledger(p.mandate), committedLedger)

	f, err := procurement.LoadAccountingFixture(accountingFixture)
	if err != nil {
		t.Fatal(err)
	}
	link, err := procurement.LinkAccountingFixtures(ctx, e.h.Admin, tenant, p.operation, "", f)
	if err != nil {
		t.Fatal(err)
	}

	// One obligation links instruction, action digest, operation, order, invoice and payment request.
	if link.InstructionID != p.instruction || link.ObligationAmount != sixLakh {
		t.Fatalf("link %+v", link)
	}
	if n := e.count(`SELECT count(*) FROM target.invoice_references i
		JOIN target.payment_requests r ON r.tenant_id = i.tenant_id AND r.invoice_ref_id = i.invoice_ref_id AND r.obligation_id = i.obligation_id
		JOIN steelmatter.obligations b ON b.tenant_id = i.tenant_id AND b.obligation_id = i.obligation_id
		JOIN steelmatter.operations o ON o.tenant_id = b.tenant_id AND o.operation_id = b.operation_id AND o.target_order_id = i.order_id
		WHERE b.instruction_id = $1 AND b.action_digest = $2 AND o.operation_id = $3 AND i.synthetic AND r.synthetic AND NOT r.bank_call`,
		p.instruction, link.ActionDigest, p.operation); n != 1 {
		t.Fatalf("%d complete order→obligation→invoice→payment chains; want 1", n)
	}
	// No threefold charge, no restoration.
	requireLedger(t, e.ledger(p.mandate), committedLedger)

	// A second invoice or payment for the same obligation is refused, as is a
	// mismatched amount or a bank call.
	if _, err := procurement.LinkAccountingFixtures(ctx, e.h.Admin, tenant, p.operation, "-again", f); err == nil ||
		!strings.Contains(err.Error(), "invoice_references_one_per_obligation") {
		t.Fatalf("second invoice for one obligation: %v", err)
	}
	if _, err := e.h.Admin.Exec(ctx, `INSERT INTO target.payment_requests (tenant_id, payment_request_id, invoice_ref_id, obligation_id, currency, amount_minor, synthetic, label)
		VALUES ($1, 'payreq-second', $2, $3, 'INR', $4, true, 'SYNTHETIC second')`, tenant, link.InvoiceRefID, link.ObligationID, sixLakh); err == nil {
		t.Fatal("second payment request for one invoice accepted")
	}
	bad := *f
	bad.Invoice.AmountMinor = 3 * sixLakh
	if _, err := procurement.LinkAccountingFixtures(ctx, e.h.Admin, tenant, p.operation, "-bad", &bad); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched invoice amount: %v", err)
	}
	if _, err := e.h.Admin.Exec(ctx, `UPDATE target.invoice_references SET amount_minor = amount_minor * 3`); err == nil {
		t.Fatal("invoice reference rewritten")
	}
	if _, err := e.h.Admin.Exec(ctx, `INSERT INTO target.payment_requests (tenant_id, payment_request_id, invoice_ref_id, obligation_id, currency, amount_minor, synthetic, label, bank_call)
		VALUES ($1, 'payreq-bank', $2, $3, 'INR', $4, true, 'SYNTHETIC bank', true)`, tenant, link.InvoiceRefID, link.ObligationID, sixLakh); err == nil {
		t.Fatal("bank call recorded")
	}

	// Neither service role can write the fixtures.
	for _, sql := range []string{
		`INSERT INTO target.invoice_references (tenant_id, invoice_ref_id, obligation_id, order_id, currency, amount_minor, synthetic, label) VALUES ('t', 'i', 'o', 'o', 'INR', 1, true, 'SYNTHETIC x')`,
		`UPDATE target.payment_requests SET amount_minor = 1`,
		`DELETE FROM target.invoice_references`,
	} {
		if _, err := e.h.Executor.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("executor: %s: %v", sql, err)
		}
		if _, err := e.h.Observer.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("observer: %s: %v", sql, err)
		}
	}

	// The committed result is unchanged on retry and the evidence names the same obligation.
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry after payment", st, body, 200, "")
	st, body = e.call("GET", "/v1/operations/"+p.operation+"/evidence", "observer-a", nil)
	requireStatus(t, "evidence", st, body, 200, "")
	if got := body["links"].(map[string]any)["obligation_id"]; got != link.ObligationID {
		t.Fatalf("evidence obligation %v, want %s", got, link.ObligationID)
	}
	if n := e.count(`SELECT count(*) FROM steelmatter.obligations WHERE instruction_id = $1`, p.instruction); n != 1 {
		t.Fatalf("%d obligations for the instruction", n)
	}

	// Paying the invoice did not replenish the allowance: a further 6,00,000 is still denied.
	inst := e.instruction("po-request-after-payment", p.mandate)
	st, body = e.evaluate("agent-a", inst["instruction_id"].(string), 1)
	requireStatus(t, "evaluate after payment", st, body, 200, "CAPACITY_EXHAUSTED")
}
