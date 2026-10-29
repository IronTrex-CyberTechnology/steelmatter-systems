// Request and response types for the Steelmatter R0 API. They mirror
// api/openapi.yaml (closed schemas) and the five payload schemas in schemas/.
// test/contract.test.ts keeps the enumerations equal to the OpenAPI document.
//
// Money and quantities are integer paise / units as JavaScript numbers. The
// API bounds every integer to 2^53-1, so numbers are exact; pass integers only.

/** Restricted ASCII identifier: ^[A-Za-z0-9][A-Za-z0-9._:-]*$, 1..128 bytes. */
export type Identifier = string;
/** Lowercase SHA-256 hex. */
export type Digest = string;
/** RFC 3339 UTC, whole seconds, Z suffix: YYYY-MM-DDTHH:MM:SSZ. */
export type Timestamp = string;
/** Compact JWS (SM-JWS-ES256-1). */
export type CompactJWS = string;

export const SCHEMA_VERSION = '1';
export const CRYPTO_PROFILE = 'SM-JWS-ES256-1';
export const CURRENCY = 'INR';
export const OPERATION = 'purchase_order.issue';

export type Currency = typeof CURRENCY;

// ---- error and deny codes (core/httpapi/codes.go) ----

export const ERROR_CODES = [
  'ACTION_CHANGED', 'ACTOR_ASSIGNMENT_INVALID', 'ACTOR_MISMATCH', 'ADAPTER_UNSUPPORTED', 'ALREADY_COMMITTED',
  'AMENDMENT_ID_CONFLICT', 'AMOUNT_MISMATCH', 'ARITHMETIC_OVERFLOW', 'AUDIENCE_MISMATCH', 'AUTHORITY_REVOKED',
  'CLOCK_UNHEALTHY', 'DELIVERY_REVISION_STALE', 'DUPLICATE_KEY', 'DUPLICATE_LINE_ITEM', 'FORBIDDEN', 'INTERNAL_ERROR', 'INVALID_ENCODING',
  'INVALID_JSON', 'INVALID_TIMESTAMP', 'INVALID_TIME_RANGE', 'JWS_ALG_UNSUPPORTED', 'JWS_HEADER_UNSUPPORTED',
  'JWS_KEY_UNTRUSTED', 'JWS_MALFORMED', 'JWS_PAYLOAD_NOT_CANONICAL', 'JWS_SIGNATURE_INVALID', 'JWS_TYPE_MISMATCH',
  'LINES_NOT_CANONICAL', 'MANDATE_EXPIRED', 'NON_INTEGER_NUMBER', 'NOT_FOUND', 'NUMBER_OUT_OF_RANGE',
  'OBSERVATION_INCONSISTENT', 'PAYLOAD_TOO_LARGE', 'PERMIT_EXPIRED', 'PERMIT_NOT_ACTIVE', 'RATE_LIMITED',
  'RETRIES_EXHAUSTED', 'REVISION_STALE', 'SCHEMA_VIOLATION', 'SERVICE_UNAVAILABLE', 'SOURCE_CONFLICT',
  'SUPPLIER_REVISION_STALE', 'SUPPLIER_UNAVAILABLE', 'UNAUTHENTICATED', 'UNKNOWN_FIELD', 'UNSUPPORTED_CURRENCY',
  'UNSUPPORTED_PROFILE', 'UNSUPPORTED_VERSION',
] as const;
export type ErrorCode = (typeof ERROR_CODES)[number];

/**
 * The one HTTP status each error code is sent with (core/httpapi/codes.go, the
 * single source; contract.test.ts keeps this equal to it). An envelope whose
 * status and code disagree is outside the contract.
 */
export const ERROR_STATUS: Readonly<Record<ErrorCode, number>> = {
  ACTION_CHANGED: 409, ACTOR_ASSIGNMENT_INVALID: 422, ACTOR_MISMATCH: 403, ADAPTER_UNSUPPORTED: 422,
  ALREADY_COMMITTED: 409, AMENDMENT_ID_CONFLICT: 409, AMOUNT_MISMATCH: 400, ARITHMETIC_OVERFLOW: 400,
  AUDIENCE_MISMATCH: 403, AUTHORITY_REVOKED: 403, CLOCK_UNHEALTHY: 503, DELIVERY_REVISION_STALE: 409, DUPLICATE_KEY: 400, DUPLICATE_LINE_ITEM: 400,
  FORBIDDEN: 403, INTERNAL_ERROR: 500, INVALID_ENCODING: 400, INVALID_JSON: 400, INVALID_TIMESTAMP: 400,
  INVALID_TIME_RANGE: 400, JWS_ALG_UNSUPPORTED: 400, JWS_HEADER_UNSUPPORTED: 400, JWS_KEY_UNTRUSTED: 403,
  JWS_MALFORMED: 400, JWS_PAYLOAD_NOT_CANONICAL: 400, JWS_SIGNATURE_INVALID: 403, JWS_TYPE_MISMATCH: 400,
  LINES_NOT_CANONICAL: 400, MANDATE_EXPIRED: 403, NON_INTEGER_NUMBER: 400, NOT_FOUND: 404, NUMBER_OUT_OF_RANGE: 400,
  OBSERVATION_INCONSISTENT: 400, PAYLOAD_TOO_LARGE: 400, PERMIT_EXPIRED: 403, PERMIT_NOT_ACTIVE: 409,
  RATE_LIMITED: 429, RETRIES_EXHAUSTED: 503, REVISION_STALE: 409, SCHEMA_VIOLATION: 400, SERVICE_UNAVAILABLE: 503,
  SOURCE_CONFLICT: 409, SUPPLIER_REVISION_STALE: 409, SUPPLIER_UNAVAILABLE: 422, UNAUTHENTICATED: 401,
  UNKNOWN_FIELD: 400, UNSUPPORTED_CURRENCY: 422, UNSUPPORTED_PROFILE: 422, UNSUPPORTED_VERSION: 422,
};

export const DENY_REASONS = [
  'APPROVAL_REQUIRED', 'APPROVAL_STALE', 'AUTHORITY_REVOKED', 'CAPACITY_EXHAUSTED', 'DELIVERY_NOT_PERMITTED', 'MANDATE_EXPIRED', 'SUPPLIER_NOT_PERMITTED',
] as const;
export type DenyReason = (typeof DENY_REASONS)[number];

/** The document 03 error envelope. */
export interface ErrorBody {
  error_code: ErrorCode;
  message: string;
  request_id: string;
  retryable: boolean;
  operation_id?: Identifier;
}

// ---- payloads (schemas/*.schema.json) ----

export interface Line {
  item_id: Identifier;
  quantity: number;
  /** Per unit_id (the product's base unit when absent), before tax. */
  unit_price_minor: number;
  /** D1 v1.4: the authorised unit of quantity; never converted. */
  unit_id?: Identifier;
  /** D1 v1.4: the authorised taxes on the line (1..10, sorted by the server). */
  tax_ids?: Identifier[];
}

export interface MandatePayload {
  schema_version: typeof SCHEMA_VERSION;
  crypto_profile: typeof CRYPTO_PROFILE;
  mandate_id: Identifier;
  tenant_id: Identifier;
  issuer_id: Identifier;
  revision: number;
  control_epoch: number;
  actor_ids: Identifier[];
  audience: Identifier;
  operation: typeof OPERATION;
  supplier_ids: Identifier[];
  delivery_ids: Identifier[];
  currency: Currency;
  limit_minor: number;
  issued_at: Timestamp;
  expires_at: Timestamp;
}

/** The canonical action. Present it to admitOperation exactly as returned. */
export interface ActionPayload {
  schema_version: typeof SCHEMA_VERSION;
  crypto_profile: typeof CRYPTO_PROFILE;
  tenant_id: Identifier;
  instruction_id: Identifier;
  action_revision: number;
  actor_id: Identifier;
  audience: Identifier;
  operation: typeof OPERATION;
  supplier_id: Identifier;
  supplier_revision: number;
  delivery_id: Identifier;
  currency: Currency;
  lines: Line[];
  /** The full total including tax: the amount held. */
  amount_minor: number;
  /** D1 v1.4: present together when the action is taxed; amount_minor = untaxed + tax. */
  amount_untaxed_minor?: number;
  tax_minor?: number;
  /** D1 v1.4: the destination's protected delivery address revision when authorised. */
  delivery_revision?: number;
}

export interface PermitPayload {
  schema_version: typeof SCHEMA_VERSION;
  crypto_profile: typeof CRYPTO_PROFILE;
  permit_id: Identifier;
  reservation_id: Identifier;
  operation_id: Identifier;
  tenant_id: Identifier;
  instruction_id: Identifier;
  action_revision: number;
  action_digest: Digest;
  mandate_id: Identifier;
  mandate_revision: number;
  mandate_digest: Digest;
  control_epoch: number;
  actor_id: Identifier;
  audience: Identifier;
  currency: Currency;
  amount_minor: number;
  issued_at: Timestamp;
  expires_at: Timestamp;
}

// ---- mandates ----

/** tenant, owner, mandate_id, revision, control_epoch and issued_at are server-owned. */
export interface MandateCreateRequest {
  actor_ids: Identifier[];
  audience: Identifier;
  supplier_ids: Identifier[];
  delivery_ids: Identifier[];
  currency: Currency;
  limit_minor: number;
  expires_at: Timestamp;
}

/** limit = available + reserved + committed + unresolved, all integer paise. */
export interface Buckets {
  limit_minor: number;
  available_minor: number;
  reserved_minor: number;
  committed_minor: number;
  unresolved_minor: number;
}

export interface MandateResponse {
  mandate: MandatePayload;
  mandate_digest: Digest;
  mandate_jws: CompactJWS;
  status: 'ACTIVE' | 'REVOKED';
  buckets: Buckets;
}

export interface RevokeResponse {
  mandate_id: Identifier;
  status: 'REVOKED';
  control_epoch: number;
  revoked_at: Timestamp;
  buckets: Buckets;
  /** R1 operations dispatched before the revocation and still PENDING or UNRESOLVED; their amount stays held until settled. */
  in_flight: Identifier[];
}

// ---- instructions ----

/** Material fields an issuer may propose; server-owned fields are rejected. */
export interface ProposedAction {
  supplier_id: Identifier;
  delivery_id: Identifier;
  currency: Currency;
  lines: Line[];
  /**
   * D1 v1.4: required when any line has tax_ids, refused otherwise. The
   * destination recomputes it at create and refuses any difference.
   */
  tax_minor?: number;
}

export interface InstructionCreateRequest {
  source_system: Identifier;
  source_record_id: Identifier;
  mandate_id: Identifier;
  proposed_action: ProposedAction;
  /** Correlation only; never part of identity or idempotency. */
  client_request_id?: Identifier;
}

export interface InstructionResponse {
  instruction_id: Identifier;
  source_system: Identifier;
  source_record_id: Identifier;
  mandate_id: Identifier;
  actor_id: Identifier;
  action_revision: number;
  action: ActionPayload;
  action_digest: Digest;
  status: 'OPEN' | 'COMMITTED';
}

export interface AmendmentRequest {
  amendment_id: Identifier;
  expected_action_revision: number;
  proposed_action: ProposedAction;
}

export interface AmendmentResponse {
  amendment_id: Identifier;
  instruction_id: Identifier;
  previous_action_revision: number;
  action_revision: number;
  action: ActionPayload;
  action_digest: Digest;
  invalidated_permit_ids: Identifier[];
  released_minor: number;
}

// ---- actions ----

export interface EvaluateRequest {
  instruction_id: Identifier;
  action_revision: number;
}

export interface PermitIssued {
  decision: 'PERMIT_ISSUED';
  permit: PermitPayload;
  permit_jws: CompactJWS;
  permit_digest: Digest;
  action: ActionPayload;
  action_digest: Digest;
}

/** A policy denial: HTTP 200 with a reason, never an error. */
export interface Denied {
  decision: 'DENY';
  reason: DenyReason;
  message: string;
  instruction_id: Identifier;
  action_revision: number;
}

export type EvaluateResponse = PermitIssued | Denied;

export interface SimulationResult {
  simulation: true;
  decision: 'PERMIT_WOULD_ISSUE' | 'DENY';
  reason?: DenyReason;
}

// ---- operations ----

/** The exact permitted action and its permit. There is no destination field. */
export interface OperationRequest {
  action: ActionPayload;
  permit_jws: CompactJWS;
}

export interface TargetRef {
  adapter_id: Identifier;
  order_id: Identifier;
}

export interface OperationResult {
  operation_id: Identifier;
  instruction_id: Identifier;
  action_revision: number;
  action_digest: Digest;
  execution: 'COMMITTED';
  obligation_id: Identifier;
  target_ref: TargetRef;
  committed_at: Timestamp;
}

export type AuthorisationStatus = 'PERMITTED' | 'DENIED' | 'REVOKED' | 'EXPIRED';
/**
 * COMMITTED is the only success value. R1 dispatched operations may also be
 * PENDING (dispatch recorded, send in progress), UNRESOLVED (outcome unknown;
 * lookup and fence in progress), FENCED (destination proved no order;
 * allowance released), MISMATCH (an order exists but differs; held) or HELD
 * (blocked over an earlier create attempt whose outcome is not established;
 * held for an operator finding).
 */
export type ExecutionStatus = 'NOT_ADMITTED' | 'PENDING' | 'UNRESOLVED' | 'COMMITTED' | 'FENCED' | 'MISMATCH' | 'HELD' | 'REJECTED';
export type ObservationStatus = 'NOT_OBSERVED' | 'MATCH' | 'MISMATCH' | 'INSUFFICIENT_EVIDENCE';
export type EvidenceStatus = 'PENDING' | 'AVAILABLE' | 'INVALID';

/** Four independent status fields (document 03). */
export interface OperationStatus {
  operation_id: Identifier;
  instruction_id: Identifier;
  authorisation: AuthorisationStatus;
  execution: ExecutionStatus;
  observation: ObservationStatus;
  evidence: EvidenceStatus;
  target_ref?: TargetRef;
}

export interface ReconcileResult {
  operation_id: Identifier;
  state: 'QUEUED' | 'RESOLVED';
  observation: ObservationStatus;
}

/**
 * Signed records exactly as issued, the action they bind and the service's
 * linkage claims. No trust anchors: verify with @irontrex/steelmatter-verifier
 * and your own trust store; do not trust `evidence` or `links` on their own.
 */
export interface EvidenceBundle {
  operation_id: Identifier;
  evidence: EvidenceStatus;
  action: ActionPayload;
  records: {
    mandate_jws?: CompactJWS;
    permit_jws?: CompactJWS;
    execution_jws?: CompactJWS;
    observation_jws?: CompactJWS;
  };
  links: {
    instruction_id: Identifier;
    action_digest: Digest;
    mandate_digest?: Digest;
    permit_digest?: Digest;
    execution_digest?: Digest;
    obligation_id?: Identifier;
  };
  incomplete: string[];
}

// ---- adapters ----

export interface AdapterAssurance {
  adapter_id: Identifier;
  version: string;
  simulation: boolean;
  supported_operations: (typeof OPERATION)[];
  supported_paths: string[];
  limitations: string[];
  test_references: string[];
}

// ---- R1 dispatches, repairs and recovery ----

export type DispatchExecution = 'PENDING' | 'UNRESOLVED' | 'COMMITTED' | 'FENCED' | 'MISMATCH' | 'HELD';

/** Stored state of an R1 dispatched operation (op_ref = the permit's operation ID). */
export interface DispatchResult {
  op_ref: Identifier;
  instruction_id: Identifier;
  action_revision: number;
  action_digest: Digest;
  execution: DispatchExecution;
  last_outcome?: string;
  destination_order?: string;
}

export interface FindingRequest {
  finding: string;
  evidence_ref: string;
}

export interface FindingResult {
  op_ref: Identifier;
  anchor_seq: number;
}

/**
 * COMMIT or RELEASE move a HELD or MISMATCH amount. RELEASE is refused while
 * the recovery anchor records a create attempt (R1 decision 3). ANCHOR_REPAIR
 * re-appends a missing anchor intent and moves no budget (R1 decision 4).
 */
export type RepairOutcome = 'COMMIT' | 'RELEASE' | 'ANCHOR_REPAIR';

export interface RepairRequest {
  outcome: RepairOutcome;
  evidence_ref: string;
  summary?: string;
}

export interface ReviewRequest {
  approved: boolean;
  note?: string;
}

export interface RepairResult {
  op_ref: Identifier;
  repair_id: Identifier;
  outcome?: RepairOutcome;
  approved?: boolean;
}

/** One approver's decision on the exact current action revision (T17-T19). */
export interface ApprovalRequest {
  action_revision: number;
  action_digest: string;
  approved: boolean;
  note?: string;
}

export interface ApprovalResult {
  approval_id: Identifier;
  instruction_id: Identifier;
  action_revision: number;
  action_digest: string;
  decision: 'APPROVED' | 'REJECTED';
  approver_id: Identifier;
}

/** Confirms a prepared and reviewed ANCHOR_REPAIR (three different people). */
export interface AnchorRepairRequest {
  repair_id: Identifier;
}

export interface AnchorRepairResult {
  op_ref: Identifier;
  repair_id: Identifier;
  repaired: true;
}

/** The instance write switch, read from the recovery anchor. */
export interface WriteState {
  enabled: boolean;
  generation: number;
  reason: string;
  baseline_seq: number;
}

export type RestoreSignalCode = 'ENGINE_MISSING_OPERATION' | 'ANCHOR_MISSING_OPERATION' | 'ENGINE_STATUS_BEHIND' | 'DESTINATION_BEHIND'
  | 'UNKNOWN_DESTINATION_OPERATION' | 'COMMITTED_ORDER_MISSING';

export interface RestoreSignal {
  code: RestoreSignalCode;
  tenant_id?: Identifier;
  op_ref?: Identifier;
  detail: string;
}

export interface RestoreReport {
  signals: RestoreSignal[];
  /** True when this check turned writes off. */
  writes_disabled: boolean;
  write_state: WriteState;
  /** Committed operations flagged because their order was deleted at the destination; they stay committed. */
  flagged: Identifier[];
  /** Dispatch intents whose engine commit never happened (not a restore); the client may retry. */
  unconfirmed: Identifier[];
}

export interface ReconcileReport {
  recreated: Identifier[];
  from_odoo: Identifier[];
  flagged: Identifier[];
  refenced: Identifier[];
  /** "op_ref=STATUS" for each operation this pass brought to a final state. */
  settled: string[];
  problems: string[];
  /** Dispatch intents whose engine commit never happened; left for the client to retry. */
  unconfirmed: Identifier[];
  /** False when the pass stopped at its time budget; run reconciliation again. */
  complete: boolean;
}

export type GateCheckName = 'WRITES_ALREADY_ON' | 'ALL_READABLE' | 'ALL_OPERATIONS_PRESENT' | 'NONE_PENDING_OR_UNRESOLVED' | 'HELD_RULE'
  | 'BUCKETS_RECONCILE' | 'ANCHOR_NOT_BEHIND' | 'NO_UNEXPLAINED_SIGNALS';

export interface GateCheck {
  name: GateCheckName;
  passed: boolean;
  detail?: string;
}

export interface GateReport {
  checks: GateCheck[];
  passed: boolean;
  resumed: boolean;
  write_state: WriteState;
}
