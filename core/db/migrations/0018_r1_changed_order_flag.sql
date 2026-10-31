-- 0018 R1: a committed order edited or cancelled at the destination (R1-DEF-13).
--
-- ORDER_CHANGED_AT_DESTINATION marks a committed operation whose Odoo order
-- was edited or cancelled after it was committed: the destination journal
-- records po_edited or po_cancelled, or the observer's signed readback no
-- longer matches the authorised action. It is not a restore and writes stay
-- on. The operation stays committed with no refund (an external change can
-- never release money). The flag is never cleared; a later deletion replaces
-- it with ORDER_DELETED_AT_DESTINATION.

ALTER TABLE steelmatter.dispatches DROP CONSTRAINT IF EXISTS dispatches_flag_check;
ALTER TABLE steelmatter.dispatches ADD CONSTRAINT dispatches_flag_check
    CHECK (flag IN ('ORDER_MISSING_AFTER_RESTORE', 'ORDER_DELETED_AT_DESTINATION', 'ORDER_CHANGED_AT_DESTINATION'));

-- The guard still sets a flag once and never clears it; the only change is
-- that ORDER_DELETED_AT_DESTINATION may replace ORDER_CHANGED_AT_DESTINATION.
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
    IF NEW.repair_ref IS DISTINCT FROM OLD.repair_ref AND NOT (repaired OR (OLD.status = NEW.status AND NEW.status IN ('COMMITTED', 'FENCED') AND OLD.repair_ref IS NULL AND NEW.repair_ref IS NOT NULL)) THEN
        RAISE EXCEPTION 'dispatch %: a repair reference is set only by a confirmed repair of a HELD or MISMATCH operation', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    IF NEW.flag IS DISTINCT FROM OLD.flag AND NOT (NEW.status = 'COMMITTED' AND (OLD.flag IS NULL OR
            (OLD.flag = 'ORDER_CHANGED_AT_DESTINATION' AND NEW.flag = 'ORDER_DELETED_AT_DESTINATION'))) THEN
        RAISE EXCEPTION 'dispatch %: the flag is set once on a committed operation and never cleared (only a deletion replaces a change flag)', OLD.op_ref USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
