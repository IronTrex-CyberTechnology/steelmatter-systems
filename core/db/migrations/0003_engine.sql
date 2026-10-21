-- 0003 engine support: synthetic principal credentials and the synthetic
-- outbox consumer.

-- Synthetic bearer tokens: only the SHA-256 hex of a token is stored.
ALTER TABLE steelmatter.principals ADD COLUMN token_sha256 text UNIQUE CHECK (token_sha256 ~ '^[a-f0-9]{64}$');

-- Synthetic downstream consumer for outbox events. Delivery is at-least-once;
-- the consumer deduplicates by event_id and counts deliveries so tests can
-- observe redelivery.
CREATE TABLE steelmatter.event_receipts (
    event_id          text PRIMARY KEY,
    tenant_id         text NOT NULL,
    event_type        text NOT NULL,
    aggregate_id      text NOT NULL,
    first_received_at timestamptz NOT NULL,
    deliveries        integer NOT NULL CHECK (deliveries >= 1)
);

GRANT SELECT, INSERT, UPDATE ON steelmatter.event_receipts TO steelmatter_executor;
GRANT SELECT ON steelmatter.event_receipts TO steelmatter_observer;
