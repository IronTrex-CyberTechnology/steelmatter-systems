-- 0007 accounting continuity fixtures (T29, document 01: "R0 includes only
-- labelled synthetic invoice/payment-reference fixtures and makes no bank
-- call"; document 02: "Paying an invoice does not replenish a cumulative
-- purchasing allowance").
--
-- These rows model the downstream accounts-payable side of the synthetic
-- destination. They are loaded as trusted fixtures by the database
-- administrator (examples/procurement LinkAccountingFixtures); neither the
-- executor nor the observer is granted anything on them, and no engine path
-- reads them. They reference the one procurement obligation created at
-- commit and never touch the allowance ledger.

CREATE TABLE target.invoice_references (
    tenant_id      text   NOT NULL,
    invoice_ref_id text   NOT NULL,
    obligation_id  text   NOT NULL,
    order_id       text   NOT NULL,
    currency       text   NOT NULL CHECK (currency = 'INR'),
    amount_minor   bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    synthetic      boolean NOT NULL CHECK (synthetic),
    label          text   NOT NULL CHECK (label LIKE 'SYNTHETIC %'),
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, invoice_ref_id),
    -- R0 has no partial execution: one invoice reference per obligation.
    CONSTRAINT invoice_references_one_per_obligation UNIQUE (tenant_id, obligation_id),
    FOREIGN KEY (tenant_id, obligation_id) REFERENCES steelmatter.obligations (tenant_id, obligation_id),
    FOREIGN KEY (tenant_id, order_id) REFERENCES target.purchase_orders (tenant_id, order_id)
);

CREATE TABLE target.payment_requests (
    tenant_id          text   NOT NULL,
    payment_request_id text   NOT NULL,
    invoice_ref_id     text   NOT NULL,
    obligation_id      text   NOT NULL,
    currency           text   NOT NULL CHECK (currency = 'INR'),
    amount_minor       bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    synthetic          boolean NOT NULL CHECK (synthetic),
    label              text   NOT NULL CHECK (label LIKE 'SYNTHETIC %'),
    -- Money movement belongs to a separate DigiAuth integration; R0 never calls a bank.
    bank_call          boolean NOT NULL DEFAULT false CHECK (NOT bank_call),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, payment_request_id),
    CONSTRAINT payment_requests_one_per_invoice UNIQUE (tenant_id, invoice_ref_id),
    FOREIGN KEY (tenant_id, invoice_ref_id) REFERENCES target.invoice_references (tenant_id, invoice_ref_id),
    FOREIGN KEY (tenant_id, obligation_id) REFERENCES steelmatter.obligations (tenant_id, obligation_id)
);

-- Linkage: an invoice reference names the obligation's own order and amount;
-- a payment request names its invoice's obligation and amount. Anything else
-- would be a second, independent charge.
CREATE FUNCTION target.check_accounting_linkage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    want_order  text;
    want_amount bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'accounting fixtures are insert-only' USING ERRCODE = 'SM002';
    END IF;
    IF TG_TABLE_NAME = 'invoice_references' THEN
        SELECT o.target_order_id, b.amount_minor INTO want_order, want_amount
          FROM steelmatter.obligations b
          JOIN steelmatter.operations o ON o.tenant_id = b.tenant_id AND o.operation_id = b.operation_id
         WHERE b.tenant_id = NEW.tenant_id AND b.obligation_id = NEW.obligation_id;
        IF want_order IS DISTINCT FROM NEW.order_id OR want_amount IS DISTINCT FROM NEW.amount_minor THEN
            RAISE EXCEPTION 'invoice reference does not match its obligation order and amount' USING ERRCODE = 'SM002';
        END IF;
    ELSE
        SELECT i.amount_minor INTO want_amount FROM target.invoice_references i
         WHERE i.tenant_id = NEW.tenant_id AND i.invoice_ref_id = NEW.invoice_ref_id AND i.obligation_id = NEW.obligation_id;
        IF want_amount IS DISTINCT FROM NEW.amount_minor THEN
            RAISE EXCEPTION 'payment request does not match its invoice obligation and amount' USING ERRCODE = 'SM002';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER invoice_references_linkage BEFORE INSERT OR UPDATE OR DELETE ON target.invoice_references
    FOR EACH ROW EXECUTE FUNCTION target.check_accounting_linkage();
CREATE TRIGGER payment_requests_linkage BEFORE INSERT OR UPDATE OR DELETE ON target.payment_requests
    FOR EACH ROW EXECUTE FUNCTION target.check_accounting_linkage();

REVOKE ALL ON target.invoice_references, target.payment_requests FROM PUBLIC;
