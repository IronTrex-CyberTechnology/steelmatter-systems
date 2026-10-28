-- R1 evidence is kept apart from R0's synthetic target foreign keys.
-- Admission time survives restore; destination facts are retained at settlement.
ALTER TABLE steelmatter.dispatches ADD COLUMN admitted_at timestamptz,
    ADD COLUMN destination_facts jsonb;
UPDATE steelmatter.dispatches SET admitted_at = created_at WHERE recovered_at IS NULL;
GRANT UPDATE(destination_facts) ON steelmatter.dispatches TO steelmatter_executor;
GRANT SELECT(tenant_id,op_ref,action_canonical,company_ref,permit_digest)
    ON steelmatter.dispatches TO steelmatter_observer;

CREATE FUNCTION steelmatter.guard_dispatch_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.admitted_at IS DISTINCT FROM OLD.admitted_at THEN
        RAISE EXCEPTION 'dispatch admission time is immutable' USING ERRCODE = 'SM001';
    END IF;
    IF NEW.destination_facts IS DISTINCT FROM OLD.destination_facts AND NOT
        (OLD.destination_facts IS NULL AND OLD.status IN ('PENDING','UNRESOLVED') AND NEW.status = 'COMMITTED') THEN
        RAISE EXCEPTION 'confirmed destination facts are immutable' USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER dispatch_evidence_guard BEFORE UPDATE ON steelmatter.dispatches
    FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_dispatch_evidence();

CREATE TABLE steelmatter.r1_executions (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    instruction_id text NOT NULL,
    action_revision bigint NOT NULL,
    permit_id text NOT NULL,
    action_digest text NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    status text NOT NULL CHECK (status = 'COMMITTED'),
    target_order_id text NOT NULL,
    obligation_id text NOT NULL,
    execution_canonical bytea NOT NULL,
    execution_digest text NOT NULL CHECK (execution_digest ~ '^[a-f0-9]{64}$'),
    execution_jws text,
    committed_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id,operation_id),
    UNIQUE (tenant_id,instruction_id),
    UNIQUE (tenant_id,permit_id),
    FOREIGN KEY (tenant_id,operation_id) REFERENCES steelmatter.dispatches(tenant_id,op_ref),
    FOREIGN KEY (tenant_id,obligation_id) REFERENCES steelmatter.obligations(tenant_id,obligation_id)
);
CREATE FUNCTION steelmatter.guard_r1_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'execution_jws') IS DISTINCT FROM (to_jsonb(OLD) - 'execution_jws') OR
       (OLD.execution_jws IS NOT NULL AND NEW.execution_jws IS DISTINCT FROM OLD.execution_jws) THEN
        RAISE EXCEPTION 'R1 execution evidence is immutable' USING ERRCODE = 'SM001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER r1_execution_guard BEFORE UPDATE ON steelmatter.r1_executions
    FOR EACH ROW EXECUTE FUNCTION steelmatter.guard_r1_execution();

CREATE TABLE steelmatter.r1_observations (
    tenant_id text NOT NULL,
    observation_id text NOT NULL,
    operation_id text NOT NULL,
    status text NOT NULL CHECK(status IN ('MATCH','MISMATCH','INSUFFICIENT_EVIDENCE')),
    expected_action_digest text NOT NULL CHECK (expected_action_digest ~ '^[a-f0-9]{64}$'),
    observed_action_digest text CHECK (observed_action_digest ~ '^[a-f0-9]{64}$'),
    observation_canonical bytea NOT NULL,
    observation_digest text NOT NULL CHECK (observation_digest ~ '^[a-f0-9]{64}$'),
    observation_jws text NOT NULL,
    observed_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(tenant_id,observation_id),
    FOREIGN KEY(tenant_id,operation_id) REFERENCES steelmatter.r1_executions(tenant_id,operation_id),
    CHECK((status='INSUFFICIENT_EVIDENCE' AND observed_action_digest IS NULL) OR
          (status IN ('MATCH','MISMATCH') AND observed_action_digest IS NOT NULL))
);
CREATE INDEX r1_observations_latest ON steelmatter.r1_observations(tenant_id,operation_id,recorded_at DESC);
CREATE VIEW steelmatter.evidence_operations AS
    SELECT *, false AS external_destination FROM steelmatter.operations
    UNION ALL SELECT *, true AS external_destination FROM steelmatter.r1_executions;
CREATE VIEW steelmatter.evidence_observations AS
    SELECT * FROM steelmatter.observations UNION ALL SELECT * FROM steelmatter.r1_observations;
GRANT SELECT, INSERT ON steelmatter.r1_executions, steelmatter.r1_observations TO steelmatter_executor;
GRANT UPDATE(execution_jws) ON steelmatter.r1_executions TO steelmatter_executor;
GRANT SELECT ON steelmatter.r1_executions TO steelmatter_observer;
GRANT SELECT ON steelmatter.evidence_operations, steelmatter.evidence_observations TO steelmatter_executor;
