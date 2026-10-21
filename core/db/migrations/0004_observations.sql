-- 0004 observations: the observer's signed readback statements, kept apart
-- from the executor's execution record (I9). The observer role reads; the
-- service stores the observer-signed record. Only a key with purpose
-- SM-OBSERVATION+JWS can produce a verifiable observation.

CREATE TABLE steelmatter.observations (
    tenant_id              text NOT NULL,
    observation_id         text NOT NULL,
    operation_id           text NOT NULL,
    status                 text NOT NULL CHECK (status IN ('MATCH', 'MISMATCH', 'INSUFFICIENT_EVIDENCE')),
    expected_action_digest text NOT NULL CHECK (expected_action_digest ~ '^[a-f0-9]{64}$'),
    observed_action_digest text CHECK (observed_action_digest ~ '^[a-f0-9]{64}$'),
    observation_canonical  bytea NOT NULL,
    observation_digest     text NOT NULL CHECK (observation_digest ~ '^[a-f0-9]{64}$'),
    observation_jws        text NOT NULL,
    observed_at            timestamptz NOT NULL, -- signed payload time (whole seconds)
    -- Full-precision insertion time: orders observations within one second.
    recorded_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, observation_id),
    FOREIGN KEY (tenant_id, operation_id) REFERENCES steelmatter.operations (tenant_id, operation_id),
    CONSTRAINT observation_status_digest CHECK (
        (status = 'MATCH' AND observed_action_digest = expected_action_digest) OR
        (status = 'MISMATCH' AND observed_action_digest IS NOT NULL AND observed_action_digest <> expected_action_digest) OR
        (status = 'INSUFFICIENT_EVIDENCE' AND observed_action_digest IS NULL))
);
CREATE INDEX observations_latest ON steelmatter.observations (tenant_id, operation_id, recorded_at DESC);

GRANT SELECT, INSERT ON steelmatter.observations TO steelmatter_executor;
GRANT SELECT ON steelmatter.observations TO steelmatter_observer;
