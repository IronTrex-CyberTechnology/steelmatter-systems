-- 0002 core schema (I02 proposal; reviewed with the locking prototype).
--
-- Global lock order for every mutation (document 02):
--   1 steelmatter.mandates          (control row; also holds the allowance buckets)
--   2 steelmatter.instructions
--   3 steelmatter.action_revisions
--   4 steelmatter.issuances         (reservation + permit, one row)
--   5 target.suppliers (FOR SHARE) / target writes
-- Multiple rows of one type are locked in primary-key order. Absent rows are
-- created under earlier locks and protected by the unique constraints below.
-- Money is integer paise in bigint; every value is bounded by 2^53-1.

-- Principals and trusted configuration ------------------------------------

CREATE TABLE steelmatter.principals (
    principal_id text PRIMARY KEY,
    tenant_id    text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('OWNER', 'ISSUER', 'ACTOR', 'OBSERVER', 'OPERATOR')),
    enabled      boolean NOT NULL DEFAULT true
);

-- Which source systems an issuer may create/amend instructions for.
CREATE TABLE steelmatter.issuer_entitlements (
    tenant_id        text NOT NULL,
    issuer_principal text NOT NULL REFERENCES steelmatter.principals (principal_id),
    source_system    text NOT NULL,
    PRIMARY KEY (tenant_id, issuer_principal, source_system)
);

-- Mandates and the allowance ledger ----------------------------------------

CREATE TABLE steelmatter.mandates (
    tenant_id         text   NOT NULL,
    mandate_id        text   NOT NULL,
    revision          bigint NOT NULL CHECK (revision >= 1),
    control_epoch     bigint NOT NULL CHECK (control_epoch >= 1 AND control_epoch <= 9007199254740991),
    status            text   NOT NULL CHECK (status IN ('ACTIVE', 'REVOKED')),
    owner_principal   text   NOT NULL REFERENCES steelmatter.principals (principal_id),
    audience          text   NOT NULL,
    actor_ids         text[] NOT NULL CHECK (cardinality(actor_ids) BETWEEN 1 AND 100),
    supplier_ids      text[] NOT NULL CHECK (cardinality(supplier_ids) BETWEEN 1 AND 100),
    delivery_ids      text[] NOT NULL CHECK (cardinality(delivery_ids) BETWEEN 1 AND 100),
    currency          text   NOT NULL CHECK (currency = 'INR'),
    limit_minor       bigint NOT NULL CHECK (limit_minor BETWEEN 1 AND 9007199254740991),
    available_minor   bigint NOT NULL CHECK (available_minor >= 0),
    reserved_minor    bigint NOT NULL CHECK (reserved_minor >= 0),
    committed_minor   bigint NOT NULL CHECK (committed_minor >= 0),
    unresolved_minor  bigint NOT NULL DEFAULT 0 CHECK (unresolved_minor >= 0),
    payload_canonical bytea  NOT NULL,
    mandate_digest    text   NOT NULL CHECK (mandate_digest ~ '^[a-f0-9]{64}$'),
    mandate_jws       text,
    issued_at         timestamptz NOT NULL,
    expires_at        timestamptz NOT NULL,
    revoked_at        timestamptz,
    PRIMARY KEY (tenant_id, mandate_id),
    CONSTRAINT mandate_window CHECK (expires_at > issued_at),
    -- I3: limit = available + reserved + committed + unresolved, all nonnegative.
    CONSTRAINT mandate_buckets_conserved
        CHECK (available_minor + reserved_minor + committed_minor + unresolved_minor = limit_minor),
    CONSTRAINT mandate_revocation_consistent CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL))
);

-- Source-workflow assignment selects the executing actor. More than one
-- enabled row for a key is "ambiguous" and must be rejected by the service;
-- the table deliberately allows it so that condition is representable.
CREATE TABLE steelmatter.workflow_assignments (
    assignment_id text PRIMARY KEY,
    tenant_id     text NOT NULL,
    source_system text NOT NULL,
    mandate_id    text NOT NULL,
    actor_id      text NOT NULL REFERENCES steelmatter.principals (principal_id),
    enabled       boolean NOT NULL DEFAULT true,
    FOREIGN KEY (tenant_id, mandate_id) REFERENCES steelmatter.mandates (tenant_id, mandate_id)
);
CREATE INDEX workflow_assignments_lookup ON steelmatter.workflow_assignments (tenant_id, source_system, mandate_id) WHERE enabled;

-- Trusted instructions and action revisions --------------------------------

CREATE TABLE steelmatter.instructions (
    tenant_id               text   NOT NULL,
    instruction_id          text   NOT NULL,
    source_system           text   NOT NULL,
    source_record_id        text   NOT NULL,
    mandate_id              text   NOT NULL,
    actor_id                text   NOT NULL REFERENCES steelmatter.principals (principal_id),
    issuer_principal        text   NOT NULL REFERENCES steelmatter.principals (principal_id),
    creation_request_digest text   NOT NULL CHECK (creation_request_digest ~ '^[a-f0-9]{64}$'),
    current_revision        bigint NOT NULL CHECK (current_revision >= 1 AND current_revision <= 9007199254740991),
    created_at              timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, instruction_id),
    -- The unique trusted source key (document 02).
    CONSTRAINT instruction_source_unique UNIQUE (tenant_id, source_system, source_record_id),
    FOREIGN KEY (tenant_id, mandate_id) REFERENCES steelmatter.mandates (tenant_id, mandate_id)
);

CREATE TABLE steelmatter.action_revisions (
    tenant_id         text   NOT NULL,
    instruction_id    text   NOT NULL,
    action_revision   bigint NOT NULL CHECK (action_revision >= 1),
    status            text   NOT NULL CHECK (status IN ('CURRENT', 'SUPERSEDED', 'COMMITTED')),
    action_canonical  bytea  NOT NULL,
    action_digest     text   NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    supplier_id       text   NOT NULL,
    supplier_revision bigint NOT NULL CHECK (supplier_revision >= 1),
    delivery_id       text   NOT NULL,
    audience          text   NOT NULL,
    amount_minor      bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    created_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, instruction_id, action_revision),
    FOREIGN KEY (tenant_id, instruction_id) REFERENCES steelmatter.instructions (tenant_id, instruction_id)
);
-- Exactly one live (current or committed) revision per instruction.
CREATE UNIQUE INDEX action_revisions_one_live
    ON steelmatter.action_revisions (tenant_id, instruction_id) WHERE status IN ('CURRENT', 'COMMITTED');

CREATE TABLE steelmatter.amendments (
    tenant_id          text   NOT NULL,
    instruction_id     text   NOT NULL,
    issuer_principal   text   NOT NULL,
    amendment_id       text   NOT NULL,
    request_digest     text   NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    expected_revision  bigint NOT NULL,
    result_revision    bigint NOT NULL,
    released_minor     bigint NOT NULL CHECK (released_minor >= 0),
    created_at         timestamptz NOT NULL,
    -- amendment_id is scoped to (tenant, instruction, authenticated issuer).
    PRIMARY KEY (tenant_id, instruction_id, issuer_principal, amendment_id),
    CHECK (result_revision = expected_revision + 1),
    FOREIGN KEY (tenant_id, instruction_id, result_revision)
        REFERENCES steelmatter.action_revisions (tenant_id, instruction_id, action_revision)
);

-- Reservation + permit ("issuance") ----------------------------------------
-- State machine (diagram 02): UNSIGNED -> ACTIVE -> CONSUMED
--                              UNSIGNED/ACTIVE -> RELEASED (never reactivated)

CREATE TABLE steelmatter.issuances (
    tenant_id         text   NOT NULL,
    permit_id         text   NOT NULL,
    reservation_id    text   NOT NULL,
    operation_id      text   NOT NULL,
    instruction_id    text   NOT NULL,
    action_revision   bigint NOT NULL,
    mandate_id        text   NOT NULL,
    control_epoch     bigint NOT NULL,
    amount_minor      bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    status            text   NOT NULL CHECK (status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED', 'RELEASED')),
    release_reason    text   CHECK (release_reason IN ('EXPIRED', 'REVOKED', 'AMENDED', 'RECOVERED_UNUSED')),
    permit_canonical  bytea  NOT NULL,
    permit_digest     text   NOT NULL CHECK (permit_digest ~ '^[a-f0-9]{64}$'),
    permit_jws        text,
    issued_at         timestamptz NOT NULL,
    expires_at        timestamptz NOT NULL,
    signed_at         timestamptz,
    consumed_at       timestamptz,
    released_at       timestamptz,
    PRIMARY KEY (tenant_id, permit_id),
    CONSTRAINT issuance_reservation_unique UNIQUE (tenant_id, reservation_id),
    CONSTRAINT issuance_operation_unique UNIQUE (tenant_id, operation_id),
    CONSTRAINT issuance_window CHECK (expires_at > issued_at AND expires_at <= issued_at + interval '5 minutes'),
    CONSTRAINT issuance_signed CHECK ((status IN ('ACTIVE', 'CONSUMED')) <= (permit_jws IS NOT NULL)),
    CONSTRAINT issuance_released CHECK ((status = 'RELEASED') = (release_reason IS NOT NULL AND released_at IS NOT NULL)),
    CONSTRAINT issuance_consumed CHECK ((status = 'CONSUMED') = (consumed_at IS NOT NULL)),
    FOREIGN KEY (tenant_id, instruction_id, action_revision)
        REFERENCES steelmatter.action_revisions (tenant_id, instruction_id, action_revision),
    FOREIGN KEY (tenant_id, mandate_id) REFERENCES steelmatter.mandates (tenant_id, mandate_id)
);
-- Never double reserve: at most one live issuance per action revision.
CREATE UNIQUE INDEX issuances_one_live_per_revision
    ON steelmatter.issuances (tenant_id, instruction_id, action_revision) WHERE status IN ('UNSIGNED', 'ACTIVE', 'CONSUMED');
-- I4 at the permit level: at most one consumed permit per instruction.
CREATE UNIQUE INDEX issuances_one_consumed_per_instruction
    ON steelmatter.issuances (tenant_id, instruction_id) WHERE status = 'CONSUMED';
-- Cleanup scan.
CREATE INDEX issuances_expiry_scan ON steelmatter.issuances (expires_at) WHERE status IN ('UNSIGNED', 'ACTIVE');

-- Synthetic destination (same database, separate schema) --------------------

CREATE SCHEMA target;
REVOKE ALL ON SCHEMA target FROM PUBLIC;

CREATE TABLE target.suppliers (
    tenant_id         text   NOT NULL,
    supplier_id       text   NOT NULL,
    revision          bigint NOT NULL CHECK (revision >= 1),
    status            text   NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    -- Column-level UPDATE lets the executor take FOR SHARE locks without being
    -- able to change protected supplier data (PostgreSQL requires UPDATE on at
    -- least one column for FOR SHARE).
    last_admission_at timestamptz,
    PRIMARY KEY (tenant_id, supplier_id)
);

CREATE TABLE target.purchase_orders (
    tenant_id         text   NOT NULL,
    order_id          text   NOT NULL,
    instruction_id    text   NOT NULL,
    operation_id      text   NOT NULL,
    supplier_id       text   NOT NULL,
    supplier_revision bigint NOT NULL,
    delivery_id       text   NOT NULL,
    currency          text   NOT NULL CHECK (currency = 'INR'),
    amount_minor      bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    created_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, order_id),
    -- I4 at the destination: at most one order per trusted instruction.
    CONSTRAINT purchase_orders_one_per_instruction UNIQUE (tenant_id, instruction_id),
    CONSTRAINT purchase_orders_operation_unique UNIQUE (tenant_id, operation_id),
    FOREIGN KEY (tenant_id, supplier_id) REFERENCES target.suppliers (tenant_id, supplier_id)
);

CREATE TABLE target.purchase_order_lines (
    tenant_id        text   NOT NULL,
    order_id         text   NOT NULL,
    item_id          text   NOT NULL,
    quantity         bigint NOT NULL CHECK (quantity BETWEEN 1 AND 9007199254740991),
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor BETWEEN 0 AND 9007199254740991),
    PRIMARY KEY (tenant_id, order_id, item_id),
    FOREIGN KEY (tenant_id, order_id) REFERENCES target.purchase_orders (tenant_id, order_id)
);

-- Commit records -----------------------------------------------------------

CREATE TABLE steelmatter.obligations (
    tenant_id      text   NOT NULL,
    obligation_id  text   NOT NULL,
    instruction_id text   NOT NULL,
    operation_id   text   NOT NULL,
    action_digest  text   NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    amount_minor   bigint NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, obligation_id),
    CONSTRAINT obligations_one_per_instruction UNIQUE (tenant_id, instruction_id),
    CONSTRAINT obligations_operation_unique UNIQUE (tenant_id, operation_id)
);

CREATE TABLE steelmatter.operations (
    tenant_id       text   NOT NULL,
    operation_id    text   NOT NULL,
    instruction_id  text   NOT NULL,
    action_revision bigint NOT NULL,
    permit_id       text   NOT NULL,
    action_digest   text   NOT NULL CHECK (action_digest ~ '^[a-f0-9]{64}$'),
    status          text   NOT NULL CHECK (status = 'COMMITTED'),
    target_order_id text   NOT NULL,
    obligation_id   text   NOT NULL,
    execution_canonical bytea,
    execution_digest    text CHECK (execution_digest ~ '^[a-f0-9]{64}$'),
    execution_jws       text,
    committed_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, operation_id),
    CONSTRAINT operations_one_per_instruction UNIQUE (tenant_id, instruction_id),
    CONSTRAINT operations_permit_unique UNIQUE (tenant_id, permit_id),
    FOREIGN KEY (tenant_id, permit_id) REFERENCES steelmatter.issuances (tenant_id, permit_id),
    FOREIGN KEY (tenant_id, target_order_id) REFERENCES target.purchase_orders (tenant_id, order_id),
    FOREIGN KEY (tenant_id, obligation_id) REFERENCES steelmatter.obligations (tenant_id, obligation_id)
);

CREATE TABLE steelmatter.outbox (
    event_id     text PRIMARY KEY,
    tenant_id    text NOT NULL,
    event_type   text NOT NULL,
    aggregate_id text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL,
    delivered_at timestamptz,
    -- One event of each type per aggregate; redelivery reuses event_id.
    CONSTRAINT outbox_once UNIQUE (tenant_id, event_type, aggregate_id)
);

-- Privileges ---------------------------------------------------------------
-- Executor: read/insert/update engine and target rows; never DELETE, never
-- DDL, never update protected supplier data.
GRANT USAGE ON SCHEMA target TO steelmatter_executor, steelmatter_observer;
GRANT SELECT ON steelmatter.principals, steelmatter.issuer_entitlements, steelmatter.workflow_assignments TO steelmatter_executor;
GRANT SELECT, INSERT, UPDATE ON
    steelmatter.mandates, steelmatter.instructions, steelmatter.action_revisions, steelmatter.amendments,
    steelmatter.issuances, steelmatter.obligations, steelmatter.operations, steelmatter.outbox
    TO steelmatter_executor;
GRANT SELECT ON target.suppliers TO steelmatter_executor;
GRANT UPDATE (last_admission_at) ON target.suppliers TO steelmatter_executor;
GRANT SELECT, INSERT ON target.purchase_orders, target.purchase_order_lines TO steelmatter_executor;

-- Observer: read the destination and what it must compare against; nothing else.
GRANT SELECT ON target.suppliers, target.purchase_orders, target.purchase_order_lines TO steelmatter_observer;
GRANT SELECT ON steelmatter.action_revisions, steelmatter.operations, steelmatter.obligations TO steelmatter_observer;
