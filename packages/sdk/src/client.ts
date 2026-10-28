import { SteelmatterApiError, SteelmatterError, SteelmatterResponseError, SteelmatterTransportError } from './errors.ts';
import { ERROR_CODES, ERROR_STATUS, type ErrorCode as Code } from './types.ts';
import type {
  AdapterAssurance, AmendmentRequest, AmendmentResponse, Denied, ErrorCode, EvaluateRequest, EvidenceBundle,
  Identifier, InstructionCreateRequest, InstructionResponse, MandateCreateRequest, MandateResponse, OperationRequest,
  OperationResult, OperationStatus, PermitIssued, ReconcileResult, RevokeResponse, SimulationResult,
  AnchorRepairRequest, AnchorRepairResult, DispatchResult, FindingRequest, FindingResult, GateReport, ReconcileReport,
  RepairRequest, RepairResult, RestoreReport, ReviewRequest, WriteState,
} from './types.ts';

export interface RetryOptions {
  /** Total attempts including the first. 1 disables retries. Default 3. */
  maxAttempts: number;
  /** First backoff delay; doubles per retry with jitter. Default 100 ms. */
  baseDelayMs: number;
  /** Upper bound for one backoff delay. Default 2000 ms. */
  maxDelayMs: number;
  /** A 429 whose Retry-After exceeds this is not retried. Default 30000 ms. */
  maxRetryAfterMs: number;
}

export type TokenSource = string | (() => string | Promise<string>);

export interface ClientOptions {
  /** Service base URL, for example http://127.0.0.1:8080. */
  baseUrl: string;
  /** Synthetic principal bearer token, or a function returning the current one. */
  token: TokenSource;
  /**
   * Per-attempt timeout. Default 65000 ms: longer than the server's 60 s
   * request deadline, so the client does not give up while the server may
   * still commit.
   */
  timeoutMs?: number;
  /** Retry policy, or false to disable retries. */
  retry?: Partial<RetryOptions> | false;
  /** fetch implementation. Default globalThis.fetch. */
  fetch?: typeof fetch;
  /** Extra headers (Authorization, Content-Type and Accept cannot be overridden). */
  headers?: Record<string, string>;
  /** Backoff sleep; replaceable for tests. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** Jitter source in [0, 1); replaceable for tests. */
  random?: () => number;
}

export interface RequestOptions {
  /** Cancels the call, including any pending retry. */
  signal?: AbortSignal;
  timeoutMs?: number;
  retry?: Partial<RetryOptions> | false;
}

/** A successful response. `created` is true exactly for HTTP 201. */
export interface ApiResponse<T> {
  status: number;
  created: boolean;
  /** Server correlation ID (X-Request-Id). */
  requestId: string;
  /** Attempts made, including retries. */
  attempts: number;
  data: T;
}

/** evaluateAction result; narrow with isPermitIssued / isDenied. */
export type EvaluateApiResponse = ApiResponse<PermitIssued> | ApiResponse<Denied>;

/**
 * When a call may be repeated with the same body. Document 03 makes every
 * endpoint except mandate creation safe to repeat: identical retries return
 * the existing result (200) without another effect, and the server keeps all
 * business identities. Mandate creation has no idempotency key, so it is
 * retried only after 429, which the server returns before reading the body.
 */
type Idempotency = 'idempotent' | 'rate-limit-only';

interface Call {
  method: 'GET' | 'POST';
  path: string;
  body?: unknown;
  expect: readonly number[];
  /** Error statuses api/openapi.yaml documents for this operation; any other status is outside the contract. */
  errors: readonly number[];
  idempotency: Idempotency;
}

// Documented error statuses per operation (api/openapi.yaml; contract.test.ts
// keeps each list equal to the document).
const E_CREATE_MANDATE = [400, 401, 403, 422, 429, 500, 503] as const;
const E_WRITE_404 = [400, 401, 403, 404, 429, 500, 503] as const; // revokeMandate, reconcileOperation
const E_WRITE_409 = [400, 401, 403, 404, 409, 429, 500, 503] as const; // evaluateAction, simulateAction
const E_WRITE_409_422 = [400, 401, 403, 404, 409, 422, 429, 500, 503] as const; // createInstruction, amendInstruction, admitOperation
const E_READ = [401, 403, 404, 429, 500, 503] as const; // getOperation, getEvidence, getAdapterAssurance, getDispatch
const E_READ_STATE = [401, 403, 429, 500, 503] as const; // getWriteState
const E_RECOVERY_CHECK = [400, 401, 403, 429, 500, 503] as const; // checkRestore
const E_RECOVERY = [400, 401, 403, 409, 429, 500, 503] as const; // reconcileRestore, resumeWrites

const DEFAULT_RETRY: RetryOptions = { maxAttempts: 3, baseDelayMs: 100, maxDelayMs: 2000, maxRetryAfterMs: 30_000 };
const DEFAULT_TIMEOUT_MS = 65_000;
const MAX_RESPONSE_BYTES = 16 << 20;

/** The body was received but is unusable (too large or not UTF-8). */
class BodyError extends Error {}

function defaultSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(signal?.reason);
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

function segment(id: Identifier, name: string): string {
  if (typeof id !== 'string' || id.length === 0) throw new TypeError(`${name} must be a non-empty string`);
  // encodeURIComponent leaves '.' alone, and URL parsing (in fetch and in
  // proxies) collapses '.' and '..' segments, so /v1/operations/.. would reach
  // a different route. They are never valid identifiers.
  if (id === '.' || id === '..') throw new TypeError(`${name} must not be '.' or '..'`);
  return encodeURIComponent(id);
}

const KNOWN_ERROR_CODES: ReadonlySet<string> = new Set(ERROR_CODES);

function isErrorBody(v: unknown): v is { error_code: string; message: string; request_id: string; retryable: boolean; operation_id?: string } {
  if (typeof v !== 'object' || v === null) return false;
  const o = v as Record<string, unknown>;
  return typeof o.error_code === 'string' && typeof o.message === 'string' && typeof o.retryable === 'boolean';
}

function retryAfterMs(h: string | null): number | undefined {
  if (h === null || !/^\d{1,9}$/.test(h.trim())) return undefined;
  return Number(h.trim()) * 1000;
}

/**
 * Client for the Steelmatter R0 API (api/openapi.yaml). One method per
 * operation; method names are the OpenAPI operationIds. A client holds one
 * principal's credential: identity (tenant, actor, issuer) always comes from
 * that credential on the server, never from request bodies.
 */
export class SteelmatterClient {
  readonly baseUrl: string;
  readonly #token: TokenSource;
  readonly #timeoutMs: number;
  readonly #retry: RetryOptions;
  readonly #fetch: typeof fetch;
  readonly #headers: Record<string, string>;
  readonly #sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  readonly #random: () => number;

  constructor(options: ClientOptions) {
    let url: URL;
    try {
      url = new URL(options.baseUrl);
    } catch {
      throw new TypeError('baseUrl must be an absolute http(s) URL');
    }
    if (url.protocol !== 'http:' && url.protocol !== 'https:') throw new TypeError('baseUrl must use http or https');
    // Bearer tokens never travel over plain HTTP to another machine: http is
    // accepted only for loopback (the local reference and demos).
    if (url.protocol === 'http:' && !isLoopback(url.hostname)) {
      throw new TypeError('baseUrl must use https unless the host is loopback (localhost, 127.0.0.0/8 or [::1])');
    }
    if (url.search || url.hash || url.username || url.password) throw new TypeError('baseUrl must not contain credentials, query or fragment');
    this.baseUrl = url.href.replace(/\/+$/, '');
    if (options.token === undefined || options.token === '') throw new TypeError('token is required');
    this.#token = options.token;
    this.#timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    this.#retry = options.retry === false ? { ...DEFAULT_RETRY, maxAttempts: 1 } : { ...DEFAULT_RETRY, ...options.retry };
    validateRetry(this.#retry);
    if (!(this.#timeoutMs > 0)) throw new TypeError('timeoutMs must be positive');
    const f = options.fetch ?? globalThis.fetch;
    if (typeof f !== 'function') throw new TypeError('no fetch implementation available');
    this.#fetch = f;
    this.#headers = { ...options.headers };
    for (const k of Object.keys(this.#headers)) {
      if (['authorization', 'content-type', 'accept'].includes(k.toLowerCase())) throw new TypeError(`header ${k} is managed by the client`);
    }
    this.#sleep = options.sleep ?? defaultSleep;
    this.#random = options.random ?? Math.random;
  }

  // ---- mandates ----

  /** Owner creates a bounded mandate (201). Not idempotent: each call creates a new mandate. */
  async createMandate(body: MandateCreateRequest, opts?: RequestOptions): Promise<ApiResponse<MandateResponse>> {
    return this.#call({ method: 'POST', path: '/v1/mandates', body, expect: [201], errors: E_CREATE_MANDATE, idempotency: 'rate-limit-only' }, opts);
  }

  /** Owner revokes a mandate; repeated calls return 200 with the same state. */
  async revokeMandate(mandateId: Identifier, opts?: RequestOptions): Promise<ApiResponse<RevokeResponse>> {
    return this.#call({ method: 'POST', path: `/v1/mandates/${segment(mandateId, 'mandateId')}/revoke`, expect: [200], errors: E_WRITE_404, idempotency: 'idempotent' }, opts);
  }

  // ---- instructions ----

  /**
   * Trusted issuer submits a source instruction; the server assigns the actor.
   * 201 new, 200 for an identical repeat (current revision). A changed repeat
   * of the same source record throws SOURCE_CONFLICT.
   */
  async createInstruction(body: InstructionCreateRequest, opts?: RequestOptions): Promise<ApiResponse<InstructionResponse>> {
    return this.#call({ method: 'POST', path: '/v1/instructions', body, expect: [201, 200], errors: E_WRITE_409_422, idempotency: 'idempotent' }, opts);
  }

  /**
   * Authorised issuer appends a revision before commit (201). An exact
   * amendment_id retry returns the recorded result (200). Re-evaluation is
   * required afterwards; no capacity is reserved automatically.
   */
  async amendInstruction(instructionId: Identifier, body: AmendmentRequest, opts?: RequestOptions): Promise<ApiResponse<AmendmentResponse>> {
    return this.#call({ method: 'POST', path: `/v1/instructions/${segment(instructionId, 'instructionId')}/revisions`, body, expect: [201, 200], errors: E_WRITE_409_422, idempotency: 'idempotent' }, opts);
  }

  // ---- actions ----

  /**
   * Assigned actor requests a permit. 201 PERMIT_ISSUED for a new issuance, 200
   * for an identical retry (same permit, never renewed) or a DENY with a reason.
   * This changes state (it reserves capacity); use simulateAction for a dry run.
   */
  async evaluateAction(body: EvaluateRequest, opts?: RequestOptions): Promise<EvaluateApiResponse> {
    return this.#call<PermitIssued | Denied>({ method: 'POST', path: '/v1/actions/evaluate', body, expect: [201, 200], errors: E_WRITE_409, idempotency: 'idempotent' }, opts);
  }

  /** Operator asks what evaluate would decide now. Creates nothing. */
  async simulateAction(body: EvaluateRequest, opts?: RequestOptions): Promise<ApiResponse<SimulationResult>> {
    return this.#call({ method: 'POST', path: '/v1/actions/simulate', body, expect: [200], errors: E_WRITE_409, idempotency: 'idempotent' }, opts);
  }

  // ---- operations ----

  /**
   * Assigned actor presents the exact permitted action and its permit. 201
   * COMMITTED, or 200 with the stored result for an identical repeat (safe to
   * retry after a lost response). Any change to the action throws ACTION_CHANGED.
   */
  async admitOperation(body: OperationRequest, opts?: RequestOptions): Promise<ApiResponse<OperationResult>> {
    return this.#call({ method: 'POST', path: '/v1/operations', body, expect: [201, 200], errors: E_WRITE_409_422, idempotency: 'idempotent' }, opts);
  }

  /** Execution, observation and evidence status plus a scoped target reference. */
  async getOperation(operationId: Identifier, opts?: RequestOptions): Promise<ApiResponse<OperationStatus>> {
    return this.#call({ method: 'GET', path: `/v1/operations/${segment(operationId, 'operationId')}`, expect: [200], errors: E_READ, idempotency: 'idempotent' }, opts);
  }

  /** Observer/operator readback. R0 resolves synchronously (200); 202 QUEUED is reserved for R1. */
  async reconcileOperation(operationId: Identifier, opts?: RequestOptions): Promise<ApiResponse<ReconcileResult>> {
    return this.#call({ method: 'POST', path: `/v1/operations/${segment(operationId, 'operationId')}/reconcile`, expect: [200, 202], errors: E_WRITE_404, idempotency: 'idempotent' }, opts);
  }

  /** Signed records, linkage and incomplete status. Verify offline with your own trust anchors. */
  async getEvidence(operationId: Identifier, opts?: RequestOptions): Promise<ApiResponse<EvidenceBundle>> {
    return this.#call({ method: 'GET', path: `/v1/operations/${segment(operationId, 'operationId')}/evidence`, expect: [200], errors: E_READ, idempotency: 'idempotent' }, opts);
  }

  // ---- adapters ----

  /** Supported paths, limitations, version and test references of a destination adapter. */
  async getAdapterAssurance(adapterId: Identifier, opts?: RequestOptions): Promise<ApiResponse<AdapterAssurance>> {
    return this.#call({ method: 'GET', path: `/v1/adapters/${segment(adapterId, 'adapterId')}/assurance`, expect: [200], errors: E_READ, idempotency: 'idempotent' }, opts);
  }

  // ---- R1 dispatches and repairs ----

  /**
   * R1. Assigned actor dispatches the exact permitted action to the configured
   * destination. 201 recorded (PENDING), 200 stored state for an identical
   * repeat. The order is created by the settle worker; poll getDispatch.
   */
  async dispatchOperation(body: OperationRequest, opts?: RequestOptions): Promise<ApiResponse<DispatchResult>> {
    return this.#call({ method: 'POST', path: '/v1/dispatches', body, expect: [201, 200], errors: E_WRITE_409_422, idempotency: 'idempotent' }, opts);
  }

  /** R1. Observer or operator reads a dispatched operation. */
  async getDispatch(opRef: Identifier, opts?: RequestOptions): Promise<ApiResponse<DispatchResult>> {
    return this.#call({ method: 'GET', path: `/v1/dispatches/${segment(opRef, 'opRef')}`, expect: [200], errors: E_READ, idempotency: 'idempotent' }, opts);
  }

  /** R1. Operator runs one settle step now (send or lookup, then fence). Safe to repeat. */
  async settleDispatch(opRef: Identifier, opts?: RequestOptions): Promise<ApiResponse<DispatchResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/settle`, expect: [200], errors: E_WRITE_404, idempotency: 'idempotent' }, opts);
  }

  /** R1. Operator records a HELD finding. Never moves budget. Each call appends a record. */
  async recordHeldFinding(opRef: Identifier, body: FindingRequest, opts?: RequestOptions): Promise<ApiResponse<FindingResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/findings`, body, expect: [201], errors: E_WRITE_409, idempotency: 'rate-limit-only' }, opts);
  }

  /** R1. Operator prepares repair evidence (COMMIT or RELEASE). Nothing moves yet. */
  async prepareRepair(opRef: Identifier, body: RepairRequest, opts?: RequestOptions): Promise<ApiResponse<RepairResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/repairs`, body, expect: [201], errors: E_WRITE_409, idempotency: 'rate-limit-only' }, opts);
  }

  /** R1. A configured reviewer, not the preparer, approves or rejects repair evidence. */
  async reviewRepair(opRef: Identifier, repairId: Identifier, body: ReviewRequest, opts?: RequestOptions): Promise<ApiResponse<RepairResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/repairs/${segment(repairId, 'repairId')}/review`, body, expect: [200], errors: E_WRITE_409, idempotency: 'rate-limit-only' }, opts);
  }

  /** R1. A configured confirmer, distinct from preparer and reviewer, confirms; the only way a held amount moves. */
  async confirmRepair(opRef: Identifier, repairId: Identifier, opts?: RequestOptions): Promise<ApiResponse<DispatchResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/repairs/${segment(repairId, 'repairId')}/confirm`, expect: [200], errors: E_WRITE_409, idempotency: 'rate-limit-only' }, opts);
  }

  // ---- R1 recovery (instance-wide) ----

  /** R1. A configured confirmer repairs an anchor found behind the engine. */
  async confirmAnchorRepair(opRef: Identifier, body: AnchorRepairRequest, opts?: RequestOptions): Promise<ApiResponse<AnchorRepairResult>> {
    return this.#call({ method: 'POST', path: `/v1/dispatches/${segment(opRef, 'opRef')}/anchor-repair`, body, expect: [200], errors: E_WRITE_409, idempotency: 'rate-limit-only' }, opts);
  }

  /** R1. Observer or operator reads the write switch. */
  async getWriteState(opts?: RequestOptions): Promise<ApiResponse<WriteState>> {
    return this.#call({ method: 'GET', path: '/v1/recovery/write-state', expect: [200], errors: E_READ_STATE, idempotency: 'idempotent' }, opts);
  }

  /** R1. Operator runs the restore check; any signal turns writes off. */
  async checkRestore(opts?: RequestOptions): Promise<ApiResponse<RestoreReport>> {
    return this.#call({ method: 'POST', path: '/v1/recovery/check', expect: [200], errors: E_RECOVERY_CHECK, idempotency: 'idempotent' }, opts);
  }

  /** R1. Operator runs restore reconciliation while writes are off. Safe to repeat. */
  async reconcileRestore(opts?: RequestOptions): Promise<ApiResponse<ReconcileReport>> {
    return this.#call({ method: 'POST', path: '/v1/recovery/reconcile', expect: [200], errors: E_RECOVERY, idempotency: 'idempotent' }, opts);
  }

  /** R1. Operator runs the resume gate; a failed gate is 200 with passed false. */
  async resumeWrites(opts?: RequestOptions): Promise<ApiResponse<GateReport>> {
    return this.#call({ method: 'POST', path: '/v1/recovery/resume', expect: [200], errors: E_RECOVERY, idempotency: 'idempotent' }, opts);
  }

  // ---- transport ----

  // The result is typed per response; for a union T it is the union of
  // ApiResponse<member>, so isPermitIssued/isDenied narrow both branches.
  async #call<T>(call: Call, opts: RequestOptions = {}): Promise<Distribute<T>> {
    const retry = opts.retry === false ? { ...this.#retry, maxAttempts: 1 } : { ...this.#retry, ...opts.retry };
    validateRetry(retry);
    const timeoutMs = opts.timeoutMs ?? this.#timeoutMs;
    const body = call.body === undefined ? undefined : JSON.stringify(call.body);
    for (let attempt = 1; ; attempt++) {
      opts.signal?.throwIfAborted();
      let outcome: Outcome<T>;
      try {
        outcome = await this.#attempt<T>(call, body, timeoutMs, attempt, opts.signal);
      } catch (e) {
        if (opts.signal?.aborted) throw opts.signal.reason;
        throw e;
      }
      if (outcome.kind === 'ok') return outcome.response as Distribute<T>;
      const err = outcome.error;
      const last = attempt >= retry.maxAttempts;
      let delay: number | undefined;
      if (err instanceof SteelmatterApiError && err.retryable && (call.idempotency === 'idempotent' || err.status === 429)) {
        delay = err.retryAfterMs !== undefined ? err.retryAfterMs : this.#backoff(retry, attempt);
        if (err.retryAfterMs !== undefined && err.retryAfterMs > retry.maxRetryAfterMs) delay = undefined;
      } else if (err instanceof SteelmatterTransportError && call.idempotency === 'idempotent') {
        delay = this.#backoff(retry, attempt);
      }
      if (last || delay === undefined) throw err;
      await this.#sleep(delay, opts.signal);
    }
  }

  #backoff(r: RetryOptions, attempt: number): number {
    const exp = Math.min(r.maxDelayMs, r.baseDelayMs * 2 ** (attempt - 1));
    return Math.round(exp / 2 + (exp / 2) * this.#random());
  }

  async #attempt<T>(call: Call, body: string | undefined, timeoutMs: number, attempt: number, signal?: AbortSignal): Promise<Outcome<T>> {
    const token = typeof this.#token === 'function' ? await this.#token() : this.#token;
    if (typeof token !== 'string' || token === '' || /[\r\n]/.test(token)) throw new SteelmatterError('token source returned an invalid token');
    const headers: Record<string, string> = { ...this.#headers, Accept: 'application/json', Authorization: `Bearer ${token}` };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const timeout = AbortSignal.timeout(timeoutMs);
    const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;
    let res: Response | undefined;
    let text: string;
    try {
      // Never follow redirects: the bearer token must only reach the configured
      // origin. 'manual' hands the 3xx back (Node) or an opaque redirect
      // (browsers) instead of following it; either is refused below.
      const init: RequestInit = { method: call.method, headers, redirect: 'manual', signal: combined };
      if (body !== undefined) init.body = body;
      res = await this.#fetch(this.baseUrl + call.path, init);
      if (res.type === 'opaqueredirect' || (res.status >= 300 && res.status < 400)) {
        cancelQuietly(res.body);
        const where = res.headers.get('location');
        return {
          kind: 'error',
          error: new SteelmatterResponseError(
            `${call.method} ${call.path}: refused redirect (HTTP ${res.status || '3xx'}${where ? ` to ${where}` : ''}); the token is only sent to the configured origin`,
            { status: res.status, requestId: res.headers.get('x-request-id') ?? undefined }),
        };
      }
      text = await readLimited(res);
    } catch (e) {
      if (signal?.aborted) throw signal.reason;
      if (e instanceof BodyError) {
        return { kind: 'error', error: new SteelmatterResponseError(`${call.method} ${call.path}: ${e.message}`, { status: res!.status, requestId: res!.headers.get('x-request-id') ?? undefined }) };
      }
      const timedOut = timeout.aborted;
      return {
        kind: 'error',
        error: new SteelmatterTransportError(
          timedOut ? `${call.method} ${call.path} timed out after ${timeoutMs} ms` : `${call.method} ${call.path} failed: ${(e as Error)?.message ?? e}`,
          { timedOut, attempts: attempt, cause: e }),
      };
    }
    const requestId = res.headers.get('x-request-id') ?? undefined;
    let parsed: unknown;
    const type = res.headers.get('content-type') ?? '';
    if (text.length > 0) {
      if (!/^application\/json\b/i.test(type)) {
        return { kind: 'error', error: new SteelmatterResponseError(`HTTP ${res.status}: expected application/json, got ${type || 'no content type'}`, { status: res.status, requestId }) };
      }
      try {
        parsed = JSON.parse(text);
      } catch {
        return { kind: 'error', error: new SteelmatterResponseError(`HTTP ${res.status}: response is not valid JSON`, { status: res.status, requestId }) };
      }
    }
    if (call.expect.includes(res.status)) {
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return { kind: 'error', error: new SteelmatterResponseError(`HTTP ${res.status}: expected a JSON object`, { status: res.status, requestId }) };
      }
      return { kind: 'ok', response: { status: res.status, created: res.status === 201, requestId: requestId ?? '', attempts: attempt, data: parsed as T } };
    }
    // The service's error envelope is a status this operation documents,
    // carrying a known error code that is sent with exactly that status.
    // Anything else (an undocumented status for the operation, an unknown
    // code, or a status/code mismatch such as 404 + RATE_LIMITED) is a reply
    // outside the contract: it is not typed as an ErrorCode the caller can
    // switch on, and it is never retried.
    if (call.errors.includes(res.status) && isErrorBody(parsed) && KNOWN_ERROR_CODES.has(parsed.error_code) &&
        ERROR_STATUS[parsed.error_code as Code] === res.status) {
      return {
        kind: 'error',
        error: new SteelmatterApiError({
          status: res.status, code: parsed.error_code as ErrorCode, message: parsed.message,
          requestId: requestId ?? (typeof parsed.request_id === 'string' ? parsed.request_id : ''),
          retryable: parsed.retryable, operationId: typeof parsed.operation_id === 'string' ? parsed.operation_id : undefined,
          retryAfterMs: res.status === 429 ? retryAfterMs(res.headers.get('retry-after')) : undefined, attempts: attempt,
        }),
      };
    }
    let detail = '';
    if (isErrorBody(parsed)) {
      const code = JSON.stringify(parsed.error_code.slice(0, 64));
      if (!KNOWN_ERROR_CODES.has(parsed.error_code)) detail = ` with unknown error_code ${code}`;
      else if (ERROR_STATUS[parsed.error_code as Code] !== res.status) detail = ` with error_code ${code}, which is sent with HTTP ${ERROR_STATUS[parsed.error_code as Code]}`;
      else detail = ` (${res.status} is not documented for this operation)`;
    }
    return { kind: 'error', error: new SteelmatterResponseError(`unexpected HTTP ${res.status}${detail} for ${call.method} ${call.path}`, { status: res.status, requestId }) };
  }
}

type Distribute<T> = T extends unknown ? ApiResponse<T> : never;

type Outcome<T> = { kind: 'ok'; response: ApiResponse<T> } | { kind: 'error'; error: SteelmatterError };

function validateRetry(r: RetryOptions): void {
  if (!Number.isInteger(r.maxAttempts) || r.maxAttempts < 1 || r.maxAttempts > 10) throw new TypeError('retry.maxAttempts must be an integer in 1..10');
  for (const k of ['baseDelayMs', 'maxDelayMs', 'maxRetryAfterMs'] as const) {
    if (!(r[k] >= 0) || !Number.isFinite(r[k])) throw new TypeError(`retry.${k} must be a non-negative number`);
  }
}

/** Loopback hosts, where plain http is allowed: localhost, 127.0.0.0/8 and [::1]. */
export function isLoopback(hostname: string): boolean {
  return hostname === 'localhost' || hostname === '[::1]' || /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(hostname);
}

/**
 * Cancels a body or reader without waiting and without throwing. A custom
 * fetch may return a stream whose cancel() throws or rejects; that must not
 * turn an oversized (or otherwise refused) response into a transport error,
 * which would be retried.
 */
function cancelQuietly(target: { cancel(reason?: unknown): Promise<void> } | null | undefined): void {
  if (!target) return;
  try {
    void Promise.resolve(target.cancel()).catch(() => undefined);
  } catch {
    // cancel() threw synchronously: nothing more to release.
  }
}

/**
 * Reads the body while counting bytes and cancels the download as soon as it
 * exceeds MAX_RESPONSE_BYTES, so a server that streams without (or with a
 * false) Content-Length cannot make the client buffer an unbounded body.
 */
async function readLimited(res: Response): Promise<string> {
  const declared = res.headers.get('content-length');
  if (declared !== null && Number(declared) > MAX_RESPONSE_BYTES) {
    cancelQuietly(res.body);
    throw new BodyError(`response larger than ${MAX_RESPONSE_BYTES} bytes`);
  }
  if (res.body === null) return '';
  const reader = res.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_RESPONSE_BYTES) {
        cancelQuietly(reader);
        throw new BodyError(`response larger than ${MAX_RESPONSE_BYTES} bytes`);
      }
      chunks.push(value);
    }
  } finally {
    try {
      reader.releaseLock();
    } catch {
      // A custom stream may refuse; the body is abandoned either way.
    }
  }
  const buf = new Uint8Array(total);
  let at = 0;
  for (const c of chunks) {
    buf.set(c, at);
    at += c.byteLength;
  }
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(buf);
  } catch {
    throw new BodyError('response body is not valid UTF-8');
  }
}
