-- Steelmatter recovery anchor (D5 v3, decided 28 Oct 2026: option 1).
--
-- A separate PostgreSQL database with its own backup and recovery controls,
-- outside the rollback domain of both the engine database and Odoo.
-- Restoring Steelmatter or Odoo must leave this purchase history untouched.
--
-- Two append-only streams, each hash-chained (every record carries the hash
-- of the previous one in its stream, so a gap or an edit is detectable):
--   engine:      DISPATCH_INTENT, OUTCOME, WRITE_STATE, OPERATOR_FINDING,
--                REPAIR_EVIDENCE, REPAIR_REVIEWED, REPAIR_CONFIRMED
--   destination: CREATE_INTENT, BLOCK, CREATE_DONE (Odoo module)
--
-- Three roles, none of which can update or delete anything:
--   anchor_engine_writer: appends engine records; reads both streams
--   anchor_dest_writer:   appends destination records; reads that stream
--   anchor_observer:      reads the destination stream
-- Appends go only through the SECURITY DEFINER functions below, which lock
-- the stream head, so sequence and hash order equal commit order.

CREATE SCHEMA anchor;

CREATE TABLE anchor.stream_heads (
    stream    text   PRIMARY KEY CHECK (stream IN ('engine', 'destination')),
    last_seq  bigint NOT NULL,
    last_hash bytea  NOT NULL
);
INSERT INTO anchor.stream_heads VALUES ('engine', 0, '\x0000000000000000000000000000000000000000000000000000000000000000'),
                                       ('destination', 0, '\x0000000000000000000000000000000000000000000000000000000000000000');

CREATE TABLE anchor.engine_records (
    seq         bigint      PRIMARY KEY,
    record_type text        NOT NULL CHECK (record_type IN ('DISPATCH_INTENT', 'OUTCOME', 'WRITE_STATE', 'OPERATOR_FINDING',
                                                            'REPAIR_EVIDENCE', 'REPAIR_REVIEWED', 'REPAIR_CONFIRMED')),
    tenant_id   text        NOT NULL DEFAULT '',
    op_ref      text        NOT NULL DEFAULT '',
    payload     jsonb       NOT NULL,
    prev_hash   bytea       NOT NULL,
    hash        bytea       NOT NULL,
    recorded_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX engine_one_intent ON anchor.engine_records (tenant_id, op_ref) WHERE record_type = 'DISPATCH_INTENT';
CREATE INDEX engine_by_type ON anchor.engine_records (record_type, seq);
CREATE INDEX engine_by_op ON anchor.engine_records (tenant_id, op_ref, seq);

CREATE TABLE anchor.destination_records (
    seq         bigint      PRIMARY KEY,
    record_type text        NOT NULL CHECK (record_type IN ('CREATE_INTENT', 'BLOCK', 'CREATE_DONE')),
    destination text        NOT NULL,
    op_ref      text        NOT NULL,
    payload     jsonb       NOT NULL,
    prev_hash   bytea       NOT NULL,
    hash        bytea       NOT NULL,
    recorded_at timestamptz NOT NULL,
    CONSTRAINT destination_once UNIQUE (destination, op_ref, record_type)
);

-- The record hash: sha256(previous hash || the record's content).
CREATE FUNCTION anchor.record_hash(prev bytea, record_type text, scope text, op_ref text, payload jsonb) RETURNS bytea
LANGUAGE sql IMMUTABLE AS $$
    SELECT sha256(prev || convert_to(record_type || '|' || scope || '|' || op_ref || '|' || payload::text, 'UTF8'))
$$;

-- Append-only for every role, including administrators.
CREATE FUNCTION anchor.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'anchor records are append-only' USING ERRCODE = 'SM001';
END $$;
CREATE TRIGGER engine_append_only BEFORE UPDATE OR DELETE ON anchor.engine_records FOR EACH ROW EXECUTE FUNCTION anchor.refuse_change();
CREATE TRIGGER engine_no_truncate BEFORE TRUNCATE ON anchor.engine_records FOR EACH STATEMENT EXECUTE FUNCTION anchor.refuse_change();
CREATE TRIGGER destination_append_only BEFORE UPDATE OR DELETE ON anchor.destination_records FOR EACH ROW EXECUTE FUNCTION anchor.refuse_change();
CREATE TRIGGER destination_no_truncate BEFORE TRUNCATE ON anchor.destination_records FOR EACH STATEMENT EXECUTE FUNCTION anchor.refuse_change();

-- Current write state: the latest WRITE_STATE record, or enabled at
-- generation 1 when none exists yet.
CREATE FUNCTION anchor.current_write_state() RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT COALESCE((SELECT payload FROM anchor.engine_records WHERE record_type = 'WRITE_STATE' ORDER BY seq DESC LIMIT 1),
                    '{"enabled": true, "generation": 1, "reason": "initial", "baseline_seq": 0}'::jsonb)
$$;

-- Appends an engine record. With require_generation set, the append is
-- refused unless writes are enabled at exactly that generation, checked under
-- the same head lock as WRITE_STATE changes, so no dispatch intent can be
-- appended after writes were turned off. A repeated DISPATCH_INTENT with the
-- same payload returns the stored sequence; a different one is refused.
CREATE FUNCTION anchor.append_engine(p_type text, p_tenant text, p_op_ref text, p_payload jsonb, p_require_generation bigint)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, anchor AS $$
DECLARE
    h anchor.stream_heads%ROWTYPE;
    ws jsonb;
    existing anchor.engine_records%ROWTYPE;
    n bigint;
    hv bytea;
BEGIN
    IF p_type = 'WRITE_STATE' THEN
        RAISE EXCEPTION 'use anchor.set_write_state' USING ERRCODE = 'SM002';
    END IF;
    SELECT * INTO h FROM anchor.stream_heads WHERE stream = 'engine' FOR UPDATE;
    IF p_require_generation IS NOT NULL THEN
        ws := anchor.current_write_state();
        IF NOT (ws->>'enabled')::boolean OR (ws->>'generation')::bigint <> p_require_generation THEN
            RAISE EXCEPTION 'writes are disabled or the generation changed (current %)', ws USING ERRCODE = 'SM003';
        END IF;
    END IF;
    IF p_type = 'DISPATCH_INTENT' THEN
        SELECT * INTO existing FROM anchor.engine_records WHERE record_type = 'DISPATCH_INTENT' AND tenant_id = p_tenant AND op_ref = p_op_ref;
        IF FOUND THEN
            IF existing.payload = p_payload THEN
                RETURN existing.seq;
            END IF;
            RAISE EXCEPTION 'dispatch intent for % already recorded with different content', p_op_ref USING ERRCODE = 'SM004';
        END IF;
    END IF;
    n := h.last_seq + 1;
    hv := anchor.record_hash(h.last_hash, p_type, p_tenant, p_op_ref, p_payload);
    INSERT INTO anchor.engine_records VALUES (n, p_type, p_tenant, p_op_ref, p_payload, h.last_hash, hv, clock_timestamp());
    UPDATE anchor.stream_heads SET last_seq = n, last_hash = hv WHERE stream = 'engine';
    RETURN n;
END $$;

-- Changes the write state. Turning writes off bumps the generation, so no
-- worker from before can dispatch again. Turning them on requires the
-- current generation (the one the resume gate checked) and records the
-- destination baseline sequence after reconciliation.
CREATE FUNCTION anchor.set_write_state(p_enabled boolean, p_reason text, p_expect_generation bigint, p_baseline_seq bigint)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, anchor AS $$
DECLARE
    h anchor.stream_heads%ROWTYPE;
    ws jsonb;
    gen bigint;
    rec jsonb;
    n bigint;
    hv bytea;
BEGIN
    SELECT * INTO h FROM anchor.stream_heads WHERE stream = 'engine' FOR UPDATE;
    ws := anchor.current_write_state();
    gen := (ws->>'generation')::bigint;
    IF p_enabled THEN
        IF p_expect_generation IS DISTINCT FROM gen THEN
            RAISE EXCEPTION 'generation changed: expected %, current %', p_expect_generation, gen USING ERRCODE = 'SM003';
        END IF;
        IF (ws->>'enabled')::boolean THEN
            RETURN ws;
        END IF;
        rec := jsonb_build_object('enabled', true, 'generation', gen, 'reason', p_reason, 'baseline_seq', COALESCE(p_baseline_seq, 0));
    ELSE
        IF NOT (ws->>'enabled')::boolean THEN
            RETURN ws; -- already off; keep the generation and the first reason
        END IF;
        rec := jsonb_build_object('enabled', false, 'generation', gen + 1, 'reason', p_reason,
                                  'baseline_seq', COALESCE((ws->>'baseline_seq')::bigint, 0));
    END IF;
    n := h.last_seq + 1;
    hv := anchor.record_hash(h.last_hash, 'WRITE_STATE', '', '', rec);
    INSERT INTO anchor.engine_records VALUES (n, 'WRITE_STATE', '', '', rec, h.last_hash, hv, clock_timestamp());
    UPDATE anchor.stream_heads SET last_seq = n, last_hash = hv WHERE stream = 'engine';
    RETURN rec;
END $$;

-- Appends a destination record (Odoo module identity). Idempotent per
-- (destination, op_ref, record_type): a repeat returns the stored sequence.
CREATE FUNCTION anchor.append_destination(p_type text, p_destination text, p_op_ref text, p_payload jsonb)
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
    n := h.last_seq + 1;
    hv := anchor.record_hash(h.last_hash, p_type, p_destination, p_op_ref, p_payload);
    INSERT INTO anchor.destination_records VALUES (n, p_type, p_destination, p_op_ref, p_payload, h.last_hash, hv, clock_timestamp());
    UPDATE anchor.stream_heads SET last_seq = n, last_hash = hv WHERE stream = 'destination';
    RETURN n;
END $$;

REVOKE ALL ON ALL TABLES IN SCHEMA anchor FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA anchor FROM PUBLIC;
GRANT USAGE ON SCHEMA anchor TO anchor_engine_writer, anchor_dest_writer, anchor_observer;
GRANT SELECT ON anchor.engine_records, anchor.destination_records, anchor.stream_heads TO anchor_engine_writer;
GRANT EXECUTE ON FUNCTION anchor.append_engine(text, text, text, jsonb, bigint), anchor.set_write_state(boolean, text, bigint, bigint),
    anchor.current_write_state(), anchor.record_hash(bytea, text, text, text, jsonb) TO anchor_engine_writer;
GRANT SELECT ON anchor.destination_records TO anchor_dest_writer, anchor_observer;
GRANT EXECUTE ON FUNCTION anchor.append_destination(text, text, text, jsonb), anchor.record_hash(bytea, text, text, text, jsonb) TO anchor_dest_writer;
