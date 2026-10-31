-- 0019 R1: recovery may supersede an instruction made after a restore
-- (NEW-RT0EE-SEC-01).
--
-- When the engine database is restored to before an anchored dispatch, an
-- issuer may have re-created the lost instruction (a new instruction_id for
-- the same trusted source record) before the restore was detected.
-- Reconciliation rebuilds the original, anchored instruction, so the newer
-- one is marked superseded_by the original. It keeps its rows and history;
-- it only gives up the source key. Only an instruction that never spent
-- (no dispatch, no consumed permit, no synthetic operation) is superseded;
-- its open reservations are released by the same transaction.
--
-- The source key stays unique among instructions that are not superseded.
-- The index keeps the constraint's name, so a race on the key is still
-- reported as instruction_source_unique. superseded_by is set once and never
-- changed (trigger, applies to every role).

ALTER TABLE steelmatter.instructions
    ADD COLUMN superseded_by text,
    ADD COLUMN superseded_at timestamptz,
    ADD CONSTRAINT instruction_superseded_pair CHECK ((superseded_by IS NULL) = (superseded_at IS NULL)),
    ADD CONSTRAINT instruction_superseded_other CHECK (superseded_by IS NULL OR superseded_by <> instruction_id);

ALTER TABLE steelmatter.instructions DROP CONSTRAINT instruction_source_unique;
CREATE UNIQUE INDEX instruction_source_unique ON steelmatter.instructions (tenant_id, source_system, source_record_id)
    WHERE superseded_by IS NULL;

CREATE FUNCTION steelmatter.guard_instruction_superseded() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.superseded_by IS NOT NULL AND (NEW.superseded_by, NEW.superseded_at) IS DISTINCT FROM (OLD.superseded_by, OLD.superseded_at) THEN
        RAISE EXCEPTION 'instruction %: a superseded instruction stays superseded', OLD.instruction_id USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER instructions_superseded_guard BEFORE UPDATE ON steelmatter.instructions
    FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_instruction_superseded();

GRANT UPDATE (superseded_by, superseded_at) ON steelmatter.instructions TO steelmatter_executor;
