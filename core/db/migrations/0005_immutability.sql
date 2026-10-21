-- 0005 immutability of signed history (review F-17) and the issuance state
-- machine at the database level. Defence in depth for "never rewrite
-- historical signed payloads" (document 02).
--
-- 1. Column-level UPDATE grants: the executor may change only workflow
--    columns. Canonical payloads, digests, identities, amounts and windows
--    are not updatable at all by the executor role.
-- 2. Triggers (apply to every role, including administrators): signatures
--    are write-once, and an issuance follows UNSIGNED -> ACTIVE -> CONSUMED or
--    UNSIGNED/ACTIVE -> RELEASED, never back.

REVOKE UPDATE ON steelmatter.mandates, steelmatter.instructions, steelmatter.action_revisions, steelmatter.amendments,
    steelmatter.issuances, steelmatter.obligations, steelmatter.operations, steelmatter.outbox, steelmatter.event_receipts
    FROM steelmatter_executor;

GRANT UPDATE (status, control_epoch, revoked_at, available_minor, reserved_minor, committed_minor, unresolved_minor)
    ON steelmatter.mandates TO steelmatter_executor;
GRANT UPDATE (current_revision) ON steelmatter.instructions TO steelmatter_executor;
GRANT UPDATE (status) ON steelmatter.action_revisions TO steelmatter_executor;
GRANT UPDATE (status, release_reason, permit_jws, signed_at, consumed_at, released_at) ON steelmatter.issuances TO steelmatter_executor;
GRANT UPDATE (execution_jws) ON steelmatter.operations TO steelmatter_executor;
GRANT UPDATE (delivered_at) ON steelmatter.outbox TO steelmatter_executor;
GRANT UPDATE (deliveries) ON steelmatter.event_receipts TO steelmatter_executor;
-- obligations and amendments: insert-only. observations: insert-only (0004).

CREATE FUNCTION steelmatter.guard_issuance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.permit_jws IS NOT NULL AND NEW.permit_jws IS DISTINCT FROM OLD.permit_jws THEN
        RAISE EXCEPTION 'issuance %: a persisted permit signature is immutable', OLD.permit_id USING ERRCODE = 'SM001';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'UNSIGNED' AND NEW.status IN ('ACTIVE', 'RELEASED')) OR
        (OLD.status = 'ACTIVE' AND NEW.status IN ('CONSUMED', 'RELEASED'))) THEN
        RAISE EXCEPTION 'issuance %: transition % -> % is not allowed', OLD.permit_id, OLD.status, NEW.status USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER issuances_guard BEFORE UPDATE ON steelmatter.issuances FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_issuance();

CREATE FUNCTION steelmatter.guard_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.execution_jws IS NOT NULL AND NEW.execution_jws IS DISTINCT FROM OLD.execution_jws THEN
        RAISE EXCEPTION 'operation %: a persisted execution signature is immutable', OLD.operation_id USING ERRCODE = 'SM001';
    END IF;
    IF (NEW.execution_canonical, NEW.execution_digest, NEW.action_digest, NEW.target_order_id, NEW.obligation_id, NEW.permit_id)
        IS DISTINCT FROM (OLD.execution_canonical, OLD.execution_digest, OLD.action_digest, OLD.target_order_id, OLD.obligation_id, OLD.permit_id) THEN
        RAISE EXCEPTION 'operation %: committed execution fields are immutable', OLD.operation_id USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER operations_guard BEFORE UPDATE ON steelmatter.operations FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_operation();

-- mandates: payload, digest, signature and bounds never change.
CREATE FUNCTION steelmatter.guard_mandate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.payload_canonical, NEW.mandate_digest, NEW.mandate_jws, NEW.limit_minor, NEW.issued_at, NEW.expires_at,
        NEW.actor_ids, NEW.supplier_ids, NEW.delivery_ids, NEW.audience, NEW.owner_principal, NEW.revision)
        IS DISTINCT FROM (OLD.payload_canonical, OLD.mandate_digest, OLD.mandate_jws, OLD.limit_minor, OLD.issued_at, OLD.expires_at,
        OLD.actor_ids, OLD.supplier_ids, OLD.delivery_ids, OLD.audience, OLD.owner_principal, OLD.revision) THEN
        RAISE EXCEPTION 'mandate %: signed fields are immutable', OLD.mandate_id USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER mandates_guard BEFORE UPDATE ON steelmatter.mandates FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_mandate();

-- action revisions: only the status may change.
CREATE FUNCTION steelmatter.guard_action_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.action_canonical, NEW.action_digest, NEW.amount_minor, NEW.supplier_revision, NEW.supplier_id, NEW.delivery_id, NEW.audience)
        IS DISTINCT FROM (OLD.action_canonical, OLD.action_digest, OLD.amount_minor, OLD.supplier_revision, OLD.supplier_id, OLD.delivery_id, OLD.audience) THEN
        RAISE EXCEPTION 'action %/%: canonical action is immutable', OLD.instruction_id, OLD.action_revision USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER action_revisions_guard BEFORE UPDATE ON steelmatter.action_revisions FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_action_revision();
