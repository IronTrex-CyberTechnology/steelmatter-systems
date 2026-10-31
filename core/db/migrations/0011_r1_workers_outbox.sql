-- 0011 R1 workers and per-aggregate outbox delivery (D6, decided 28 Oct 2026).
--
-- 1. Two more isolated workers: settle (advances dispatched R1 operations
--    through the connector) and outbox (delivers events). A failing record is
--    backed off on its own, as for the R0 workers (0008).
-- 2. Outbox ordering per aggregate (one operation or one mandate), not
--    globally. Each event gets aggregate_seq, increasing per aggregate; the
--    dispatcher only delivers the oldest undelivered event of each aggregate,
--    so a failing event holds back later events of its own aggregate while
--    other aggregates keep flowing. No global order is promised.

ALTER TABLE steelmatter.worker_backoff DROP CONSTRAINT worker_backoff_worker_check;
ALTER TABLE steelmatter.worker_backoff ADD CONSTRAINT worker_backoff_worker_check
    CHECK (worker IN ('cleanup', 'evidence', 'observer', 'settle', 'outbox'));

ALTER TABLE steelmatter.outbox ADD COLUMN aggregate_seq bigint;

UPDATE steelmatter.outbox o SET aggregate_seq = s.seq
FROM (SELECT event_id, row_number() OVER (PARTITION BY tenant_id, aggregate_id ORDER BY created_at, event_id) AS seq
      FROM steelmatter.outbox) s
WHERE o.event_id = s.event_id;

ALTER TABLE steelmatter.outbox ALTER COLUMN aggregate_seq SET NOT NULL;
ALTER TABLE steelmatter.outbox ADD CONSTRAINT outbox_aggregate_seq_positive CHECK (aggregate_seq >= 1);
ALTER TABLE steelmatter.outbox ADD CONSTRAINT outbox_aggregate_order UNIQUE (tenant_id, aggregate_id, aggregate_seq);

-- Assigns the next aggregate_seq. The transaction-scoped advisory lock
-- serialises concurrent writers of one aggregate, so sequence order is the
-- order in which those transactions inserted. Callers never supply it.
CREATE FUNCTION steelmatter.outbox_assign_seq() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('steelmatter.outbox:' || NEW.tenant_id || ':' || NEW.aggregate_id, 0));
    SELECT COALESCE(max(aggregate_seq), 0) + 1 INTO NEW.aggregate_seq
    FROM steelmatter.outbox WHERE tenant_id = NEW.tenant_id AND aggregate_id = NEW.aggregate_id;
    RETURN NEW;
END $$;
CREATE TRIGGER outbox_assign_seq BEFORE INSERT ON steelmatter.outbox FOR EACH ROW EXECUTE FUNCTION steelmatter.outbox_assign_seq();

-- Used by the dispatcher to find each aggregate's oldest undelivered event.
CREATE INDEX outbox_undelivered ON steelmatter.outbox (tenant_id, aggregate_id, aggregate_seq) WHERE delivered_at IS NULL;
