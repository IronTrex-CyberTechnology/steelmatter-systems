import type { ErrorCode, Identifier } from './types.ts';

/** Base class of every error the SDK throws. */
export class SteelmatterError extends Error {
  override name = 'SteelmatterError';
}

/**
 * The server answered with the document 03 error envelope. Nothing was
 * changed by the request unless the code says otherwise (for example
 * RETRIES_EXHAUSTED keeps operationId for lookup).
 */
export class SteelmatterApiError extends SteelmatterError {
  override name = 'SteelmatterApiError';
  /** HTTP status (400, 401, 403, 404, 409, 422, 429, 500 or 503). */
  readonly status: number;
  readonly code: ErrorCode;
  /** Server correlation ID (req_...). Quote it when reporting a problem. */
  readonly requestId: string;
  /** True exactly for 429 and 503. */
  readonly retryable: boolean;
  /** Present when the operation identity already exists; use it to look the operation up. */
  readonly operationId: Identifier | undefined;
  /** From Retry-After on 429, in milliseconds. */
  readonly retryAfterMs: number | undefined;
  /** Attempts made, including retries. */
  readonly attempts: number;

  constructor(init: {
    status: number; code: ErrorCode; message: string; requestId: string; retryable: boolean;
    operationId?: Identifier | undefined; retryAfterMs?: number | undefined; attempts: number;
  }) {
    super(`${init.code} (HTTP ${init.status}): ${init.message}`);
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId;
    this.retryable = init.retryable;
    this.operationId = init.operationId;
    this.retryAfterMs = init.retryAfterMs;
    this.attempts = init.attempts;
  }
}

/**
 * No HTTP response was received: connection failure or timeout. For a
 * non-idempotent request the outcome is unknown; for the idempotent ones the
 * SDK has already retried with the same identities.
 */
export class SteelmatterTransportError extends SteelmatterError {
  override name = 'SteelmatterTransportError';
  readonly timedOut: boolean;
  readonly attempts: number;

  constructor(message: string, init: { timedOut: boolean; attempts: number; cause?: unknown }) {
    super(message, { cause: init.cause });
    this.timedOut = init.timedOut;
    this.attempts = init.attempts;
  }
}

/** The server replied with something outside the API contract. */
export class SteelmatterResponseError extends SteelmatterError {
  override name = 'SteelmatterResponseError';
  readonly status: number;
  readonly requestId: string | undefined;

  constructor(message: string, init: { status: number; requestId?: string | undefined }) {
    super(message);
    this.status = init.status;
    this.requestId = init.requestId;
  }
}
