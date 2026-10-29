-- 0017 R1: exact-action human approval (T17-T19, R1 decision 9).
--
-- A tenant's approval policy names the amount above which an action needs a
-- human approval before a permit is issued. An approval binds the exact
-- action (instruction, action revision and action digest), the protected
-- supplier revision and the mandate revision and control epoch it was given
-- for. Evaluation accepts it only while all of these are unchanged and its
-- approver is still entitled; otherwise it asks for a fresh decision.
--
-- Approvals are evidence: insert-only for the engine, never updated or
-- deleted by any role (the trigger also binds administrators). An approval
-- reserves nothing; capacity is still checked under the mandate lock when the
-- permit is issued, so concurrent approvals cannot exceed the allowance.
--
-- Policies and approver entitlements are trusted configuration, managed by a
-- database administrator like issuer entitlements; the engine only reads them.

CREATE TABLE steelmatter.approval_policies (
    tenant_id            text   PRIMARY KEY,
    approval_above_minor bigint NOT NULL CHECK (approval_above_minor BETWEEN 0 AND 9007199254740991)
);

CREATE TABLE steelmatter.approver_entitlements (
    tenant_id          text    NOT NULL,
    approver_principal text    NOT NULL REFERENCES steelmatter.principals (principal_id),
    enabled            boolean NOT NULL DEFAULT true,
    PRIMARY KEY (tenant_id, approver_principal)
);

CREATE TABLE steelmatter.approvals (
    approval_id        text   PRIMARY KEY,
    tenant_id          text   NOT NULL,
    instruction_id     text   NOT NULL,
    action_revision    bigint NOT NULL,
    action_digest      text   NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    supplier_revision  bigint NOT NULL CHECK (supplier_revision >= 1),
    mandate_id         text   NOT NULL,
    mandate_revision   bigint NOT NULL CHECK (mandate_revision >= 1),
    control_epoch      bigint NOT NULL CHECK (control_epoch >= 1),
    amount_minor       bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    approver_principal text   NOT NULL REFERENCES steelmatter.principals (principal_id),
    decision           text   NOT NULL CHECK (decision IN ('APPROVED', 'REJECTED')),
    note               text   CHECK (note IS NULL OR char_length(note) BETWEEN 1 AND 2000),
    decided_at         timestamptz NOT NULL,
    FOREIGN KEY (tenant_id, instruction_id, action_revision)
        REFERENCES steelmatter.action_revisions (tenant_id, instruction_id, action_revision),
    FOREIGN KEY (tenant_id, mandate_id) REFERENCES steelmatter.mandates (tenant_id, mandate_id)
);
CREATE INDEX approvals_lookup ON steelmatter.approvals (tenant_id, instruction_id, action_revision, decided_at);

CREATE FUNCTION steelmatter.guard_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'approval %: approvals are append-only', OLD.approval_id USING ERRCODE = 'SM001';
END $$;
CREATE TRIGGER approvals_guard BEFORE UPDATE OR DELETE ON steelmatter.approvals
    FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_approval();

GRANT SELECT ON steelmatter.approval_policies, steelmatter.approver_entitlements TO steelmatter_executor;
GRANT SELECT, INSERT ON steelmatter.approvals TO steelmatter_executor;
