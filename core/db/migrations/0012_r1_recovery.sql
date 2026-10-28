-- 0012 R1 recovery and repair (D5 v3, decided 28 Oct 2026).
--
-- recovered_at: the operation was recreated from the recovery anchor after
--   the engine database lost it (reconciliation case B or G). Set once at
--   insert; immutable.
-- flag: ORDER_MISSING_AFTER_RESTORE marks a committed operation whose
--   destination order is gone after an Odoo restore (case E). It stays
--   committed (no refund); the flag is set once and never cleared.
-- repair_ref: the confirmed repair that moved a HELD or MISMATCH operation.
--   A HELD or MISMATCH operation can leave that state only together with a
--   repair_ref (D5 HELD rule): Uday's evidence, Kaif's review and Aakhil's
--   confirmation are recorded in the anchor first. An operator note alone
--   never moves budget.

ALTER TABLE steelmatter.dispatches
    ADD COLUMN recovered_at timestamptz,
    ADD COLUMN flag         text CHECK (flag IN ('ORDER_MISSING_AFTER_RESTORE')),
    ADD COLUMN repair_ref   text;

CREATE OR REPLACE FUNCTION steelmatter.guard_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    repaired boolean := OLD.status IN ('HELD', 'MISMATCH') AND NEW.status IN ('COMMITTED', 'FENCED')
                        AND OLD.repair_ref IS NULL AND NEW.repair_ref IS NOT NULL;
BEGIN
    IF (NEW.tenant_id, NEW.op_ref, NEW.instruction_id, NEW.action_revision, NEW.mandate_id, NEW.company_ref, NEW.permit_id, NEW.control_epoch,
        NEW.action_canonical, NEW.action_digest, NEW.permit_digest, NEW.amount_minor, NEW.created_at, NEW.recovered_at)
        IS DISTINCT FROM (OLD.tenant_id, OLD.op_ref, OLD.instruction_id, OLD.action_revision, OLD.mandate_id, OLD.company_ref, OLD.permit_id, OLD.control_epoch,
        OLD.action_canonical, OLD.action_digest, OLD.permit_digest, OLD.amount_minor, OLD.created_at, OLD.recovered_at) THEN
        RAISE EXCEPTION 'dispatch %: recorded intent is immutable', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('UNRESOLVED', 'COMMITTED', 'FENCED', 'MISMATCH', 'HELD')) OR
        (OLD.status = 'UNRESOLVED' AND NEW.status IN ('COMMITTED', 'FENCED', 'MISMATCH', 'HELD')) OR
        repaired) THEN
        RAISE EXCEPTION 'dispatch %: transition % -> % is not allowed', OLD.op_ref, OLD.status, NEW.status USING ERRCODE = 'SM001';
    END IF;
    IF NEW.repair_ref IS DISTINCT FROM OLD.repair_ref AND NOT repaired THEN
        RAISE EXCEPTION 'dispatch %: a repair reference is set only by a confirmed repair of a HELD or MISMATCH operation', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    IF NEW.flag IS DISTINCT FROM OLD.flag AND NOT (OLD.flag IS NULL AND NEW.status = 'COMMITTED') THEN
        RAISE EXCEPTION 'dispatch %: the restore flag is set once on a committed operation and never cleared', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;

GRANT UPDATE (flag, repair_ref) ON steelmatter.dispatches TO steelmatter_executor;
