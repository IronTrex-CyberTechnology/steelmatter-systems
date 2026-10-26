-- 0008 per-record worker backoff (SEC-DEF-002, SEC-DEF-003, SEC-DEF-005).
-- The evidence, observer and cleanup workers record each record that fails
-- and skip it until next_attempt_at, so one persistently failing record
-- cannot hold back later records in any tenant or fill a worker's batch
-- window. Operational state only: nothing here is signed or part of the
-- evidence. The executor never deletes (T22), so a record that later
-- succeeds is marked resolved and its row kept as failure history.

CREATE TABLE steelmatter.worker_backoff (
    worker          text        NOT NULL CHECK (worker IN ('cleanup', 'evidence', 'observer')),
    tenant_id       text        NOT NULL,
    record_id       text        NOT NULL, -- permit_id (cleanup) or operation_id (evidence, observer)
    attempts        integer     NOT NULL CHECK (attempts >= 1),
    last_error      text        NOT NULL,
    first_failed_at timestamptz NOT NULL,
    last_failed_at  timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    resolved_at     timestamptz, -- set when the record later succeeds
    PRIMARY KEY (worker, tenant_id, record_id)
);

GRANT SELECT, INSERT, UPDATE ON steelmatter.worker_backoff TO steelmatter_executor;
