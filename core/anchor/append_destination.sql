-- Steelmatter recovery anchor: the destination append function. Run after
-- schema.sql by anchor-init, and on its own by anchor-upgrade (idempotent).

-- Appends a destination record (Odoo module identity). Idempotent per
-- (destination, op_ref, record_type): a repeat returns the stored sequence.
-- A new CREATE_INTENT is refused while engine writes are off (R1-DEF-14):
-- the module writes CREATE_INTENT before its create commits, so this is the
-- final send boundary, and a create already in flight when writes go off
-- fails closed at the destination. BLOCK and CREATE_DONE are never refused,
-- so fencing and recording an order that already exists keep working.
-- Checked under a share lock on the engine head, so it is ordered against
-- every write-switch change.
-- A CREATE_INTENT after a BLOCK for the same reference is refused too (R1
-- decision 3): a BLOCK in the anchor blocks the reference permanently, even
-- if an Odoo restore erases the module's own tombstone.
CREATE OR REPLACE FUNCTION anchor.append_destination(p_type text, p_destination text, p_op_ref text, p_payload jsonb)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, anchor AS $$
DECLARE
    h anchor.stream_heads%ROWTYPE;
    existing bigint;
    n bigint;
    hv bytea;
BEGIN
    SELECT * INTO h FROM anchor.stream_heads WHERE stream = 'destination' FOR UPDATE;
    SELECT seq INTO existing FROM anchor.destination_records WHERE destination = p_destination AND op_ref = p_op_ref AND record_type = p_type;
    IF FOUND THEN
        RETURN existing;
    END IF;
    IF p_type = 'CREATE_INTENT' THEN
        -- Serialise with set_write_state (which locks the engine head).
        PERFORM 1 FROM anchor.stream_heads WHERE stream = 'engine' FOR SHARE;
    END IF;
    IF p_type = 'CREATE_INTENT' AND EXISTS (SELECT 1 FROM anchor.destination_records
            WHERE destination = p_destination AND op_ref = p_op_ref AND record_type = 'BLOCK') THEN
        RAISE EXCEPTION 'reference % is permanently blocked; no create may start', p_op_ref USING ERRCODE = 'SM005';
    END IF;
    IF p_type = 'CREATE_INTENT' AND NOT (anchor.current_write_state()->>'enabled')::boolean THEN
        RAISE EXCEPTION 'engine writes are disabled; no new create may start' USING ERRCODE = 'SM003';
    END IF;
    n := h.last_seq + 1;
    hv := anchor.record_hash(h.last_hash, p_type, p_destination, p_op_ref, p_payload);
    INSERT INTO anchor.destination_records VALUES (n, p_type, p_destination, p_op_ref, p_payload, h.last_hash, hv, clock_timestamp());
    UPDATE anchor.stream_heads SET last_seq = n, last_hash = hv WHERE stream = 'destination';
    RETURN n;
END $$;

REVOKE ALL ON FUNCTION anchor.append_destination(text, text, text, jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION anchor.append_destination(text, text, text, jsonb) TO anchor_dest_writer;
