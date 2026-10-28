-- 0010 R1 dispatch intent (D1 v1, D2 decided 28 Oct 2026, D4 decided).
--
-- An R1 operation sends the stored action to an external destination (Odoo)
-- through a connector, outside this database's transaction. Once dispatch is
-- recorded the order may exist at the destination, so the amount moves
-- reserved -> unresolved in the same transaction (D2 move 3) and leaves
-- unresolved only when the destination confirms a matching order (-> committed)
-- or proves no order and permanently blocks the reference (-> available).
--
-- R0's steelmatter.operations (synthetic, single-transaction admission) is
-- unchanged. op_ref is the operation ID already assigned at evaluation and
-- carried in the permit.

CREATE TABLE steelmatter.dispatches (
    tenant_id          text   NOT NULL,
    op_ref             text   NOT NULL,
    instruction_id     text   NOT NULL,
    action_revision    bigint NOT NULL CHECK (action_revision >= 1),
    mandate_id         text   NOT NULL,
    company_ref        text   NOT NULL,
    permit_id          text   NOT NULL,
    control_epoch      bigint NOT NULL,
    action_canonical   bytea  NOT NULL,
    action_digest      text   NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    permit_digest      text   NOT NULL CHECK (permit_digest ~ '^[a-f0-9]{64}$'),
    amount_minor       bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    -- PENDING: intent recorded, execute not yet answered. UNRESOLVED: outcome
    -- unknown, lookup and fence in progress. COMMITTED: destination confirmed a
    -- matching order. FENCED: destination proved no order and blocked op_ref.
    -- MISMATCH: an order exists but differs; held for investigation.
    -- HELD: the destination blocked op_ref but its restore-proof journal shows
    -- an earlier create attempt, so the earlier outcome is not established
    -- (D5 v3 case 1); held until an operator process establishes it.
    status             text   NOT NULL CHECK (status IN ('PENDING', 'UNRESOLVED', 'COMMITTED', 'FENCED', 'MISMATCH', 'HELD')),
    last_outcome       text,
    attempts           integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    destination_order  text,
    destination_seq    bigint,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    settled_at         timestamptz,
    PRIMARY KEY (tenant_id, op_ref),
    CONSTRAINT dispatches_one_per_instruction UNIQUE (tenant_id, instruction_id),
    CONSTRAINT dispatches_permit_unique UNIQUE (tenant_id, permit_id),
    CONSTRAINT dispatches_settled CHECK ((status IN ('COMMITTED', 'FENCED')) = (settled_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, permit_id) REFERENCES steelmatter.issuances (tenant_id, permit_id)
);

-- Settled states are final; MISMATCH and HELD may only move on through an
-- operator process designed later, so they are final here too. Rows are never
-- deleted: the operation history is kept even after a destination order is
-- lost or deleted.
CREATE FUNCTION steelmatter.guard_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.tenant_id, NEW.op_ref, NEW.instruction_id, NEW.action_revision, NEW.mandate_id, NEW.company_ref, NEW.permit_id, NEW.control_epoch,
        NEW.action_canonical, NEW.action_digest, NEW.permit_digest, NEW.amount_minor, NEW.created_at)
        IS DISTINCT FROM (OLD.tenant_id, OLD.op_ref, OLD.instruction_id, OLD.action_revision, OLD.mandate_id, OLD.company_ref, OLD.permit_id, OLD.control_epoch,
        OLD.action_canonical, OLD.action_digest, OLD.permit_digest, OLD.amount_minor, OLD.created_at) THEN
        RAISE EXCEPTION 'dispatch %: recorded intent is immutable', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('UNRESOLVED', 'COMMITTED', 'FENCED', 'MISMATCH', 'HELD')) OR
        (OLD.status = 'UNRESOLVED' AND NEW.status IN ('COMMITTED', 'FENCED', 'MISMATCH', 'HELD'))) THEN
        RAISE EXCEPTION 'dispatch %: transition % -> % is not allowed', OLD.op_ref, OLD.status, NEW.status USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER dispatches_guard BEFORE UPDATE ON steelmatter.dispatches FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_dispatch();

-- Executor only; the observer read set (0006) is unchanged.
GRANT SELECT, INSERT ON steelmatter.dispatches TO steelmatter_executor;
GRANT UPDATE (status, last_outcome, attempts, destination_order, destination_seq, updated_at, settled_at)
    ON steelmatter.dispatches TO steelmatter_executor;
