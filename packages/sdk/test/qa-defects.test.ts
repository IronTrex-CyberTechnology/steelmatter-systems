// Regression tests for independent QA defects on the SDK.
// SDK-DEF-001: only a documented error status with a known ErrorCode is a
//              SteelmatterApiError; anything else is a SteelmatterResponseError
//              and is not retried.
// SDK-DEF-002: '.' and '..' are refused as path identifiers, so URL
//              normalisation cannot send a call to a different route.
// Custom-fetch cancellation: when a custom fetch's body refuses to cancel
//              (throws, rejects or never settles) on an oversized response,
//              the result is still a SteelmatterResponseError with no retry.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SteelmatterApiError, SteelmatterClient, SteelmatterResponseError } from '../src/index.ts';

const MiB = 1 << 20;
const LIMIT = 16 * MiB;
const envelope = (code: string, retryable = false) =>
  JSON.stringify({ error_code: code, message: `${code} message`, request_id: 'req_' + 'a'.repeat(24), retryable });

function clientWith(respond: () => Response) {
  let calls = 0;
  const fetch = (async () => {
    calls++;
    return respond();
  }) as typeof globalThis.fetch;
  const c = new SteelmatterClient({ baseUrl: 'https://api.example', token: 't', fetch, retry: { maxAttempts: 3, baseDelayMs: 0, maxDelayMs: 0, maxRetryAfterMs: 0 } });
  return { c, calls: () => calls };
}

const json = (status: number, body: string, headers: Record<string, string> = {}) =>
  () => new Response(body, { status, headers: { 'content-type': 'application/json', ...headers } });

test('SDK-DEF-001: undocumented statuses and unknown error codes are SteelmatterResponseError and not retried', async () => {
  const cases: [string, () => Response][] = [
    ['418 with a known code', json(418, envelope('NOT_FOUND'))],
    ['502 with a retryable known code', json(502, envelope('SERVICE_UNAVAILABLE', true))],
    ['504 with a retryable known code', json(504, envelope('SERVICE_UNAVAILABLE', true))],
    ['409 with an unknown code', json(409, envelope('MADE_UP_CODE'))],
    ['503 with an unknown retryable code', json(503, envelope('MADE_UP_CODE', true))],
    ['429 with an unknown retryable code', json(429, envelope('NOT_A_CODE', true), { 'retry-after': '0' })],
    ['400 with an empty code', json(400, envelope(''))],
  ];
  for (const [what, respond] of cases) {
    const { c, calls } = clientWith(respond);
    const err = await c.getOperation('op-1').then(() => assert.fail(`${what}: resolved`), (e: unknown) => e);
    assert.ok(err instanceof SteelmatterResponseError, `${what}: got ${(err as Error)?.constructor?.name}`);
    assert.ok(!(err instanceof SteelmatterApiError), what);
    assert.equal(calls(), 1, `${what}: not retried`);
  }
  // Control: a documented status with its own known code is still the API error, and retryable ones are retried.
  let { c, calls } = clientWith(json(404, envelope('NOT_FOUND')));
  await assert.rejects(c.getOperation('op-1'), (e) => e instanceof SteelmatterApiError && e.code === 'NOT_FOUND' && e.status === 404);
  assert.equal(calls(), 1);
  ({ c, calls } = clientWith(json(503, envelope('SERVICE_UNAVAILABLE', true))));
  await assert.rejects(c.getOperation('op-1'), (e) => e instanceof SteelmatterApiError && e.code === 'SERVICE_UNAVAILABLE');
  assert.equal(calls(), 3);
});

const mandate = {
  actor_ids: ['agent-a'], audience: 'synthetic-procurement', supplier_ids: ['supplier-a'], delivery_ids: ['warehouse-a'],
  currency: 'INR' as const, limit_minor: 100_000_000, expires_at: '2030-01-01T00:00:00Z',
};

test('SDK-DEF-001 retest: a status the operation does not document, or a status/code mismatch, is SteelmatterResponseError', async () => {
  const cases: [string, (c: SteelmatterClient) => Promise<unknown>, number, string][] = [
    // Statuses QA confirmed per operation: valid codes for the status, but the operation does not document it.
    ['getOperation 400', (c) => c.getOperation('op-1'), 400, 'SCHEMA_VIOLATION'],
    ['getOperation 409', (c) => c.getOperation('op-1'), 409, 'REVISION_STALE'],
    ['getOperation 422', (c) => c.getOperation('op-1'), 422, 'UNSUPPORTED_VERSION'],
    ['getEvidence 400', (c) => c.getEvidence('op-1'), 400, 'SCHEMA_VIOLATION'],
    ['getAdapterAssurance 409', (c) => c.getAdapterAssurance('a-1'), 409, 'ACTION_CHANGED'],
    ['createMandate 404', (c) => c.createMandate(mandate), 404, 'NOT_FOUND'],
    ['createMandate 409', (c) => c.createMandate(mandate), 409, 'SOURCE_CONFLICT'],
    ['revokeMandate 409', (c) => c.revokeMandate('m-1'), 409, 'REVISION_STALE'],
    // Status and code disagree (each code has exactly one status in core/httpapi/codes.go).
    ['404 + RATE_LIMITED', (c) => c.getOperation('op-1'), 404, 'RATE_LIMITED'],
    ['500 + NOT_FOUND', (c) => c.getOperation('op-1'), 500, 'NOT_FOUND'],
    ['429 + NOT_FOUND', (c) => c.getOperation('op-1'), 429, 'NOT_FOUND'],
    ['503 + RATE_LIMITED (retryable code, wrong status)', (c) => c.getOperation('op-1'), 503, 'RATE_LIMITED'],
    ['403 + NOT_FOUND', (c) => c.getOperation('op-1'), 403, 'NOT_FOUND'],
  ];
  for (const [what, run, status, code] of cases) {
    const { c, calls } = clientWith(json(status, envelope(code, status === 429 || status === 503), status === 429 ? { 'retry-after': '0' } : {}));
    const err = await run(c).then(() => assert.fail(`${what}: resolved`), (e: unknown) => e);
    assert.ok(err instanceof SteelmatterResponseError, `${what}: got ${(err as Error)?.constructor?.name}`);
    assert.ok(!(err instanceof SteelmatterApiError), what);
    assert.equal((err as SteelmatterResponseError).status, status, what);
    assert.equal(calls(), 1, `${what}: not retried`);
  }
  // Controls: each documented status with its own code stays a SteelmatterApiError.
  for (const [what, run, status, code] of [
    ['getOperation 404 NOT_FOUND', (c: SteelmatterClient) => c.getOperation('op-1'), 404, 'NOT_FOUND'],
    ['createMandate 422 UNSUPPORTED_CURRENCY', (c: SteelmatterClient) => c.createMandate(mandate), 422, 'UNSUPPORTED_CURRENCY'],
    ['createMandate 400 SCHEMA_VIOLATION', (c: SteelmatterClient) => c.createMandate(mandate), 400, 'SCHEMA_VIOLATION'],
  ] as const) {
    const { c } = clientWith(json(status, envelope(code)));
    await assert.rejects(run(c), (e) => e instanceof SteelmatterApiError && e.code === code && e.status === status, what);
  }
});

test("SDK-DEF-002: '.' and '..' are refused as path identifiers before any request", async () => {
  const { c, calls } = clientWith(json(200, '{}'));
  for (const id of ['.', '..']) {
    await assert.rejects(c.getOperation(id), TypeError, `getOperation(${id})`);
    await assert.rejects(c.getEvidence(id), TypeError, `getEvidence(${id})`);
    await assert.rejects(c.revokeMandate(id), TypeError, `revokeMandate(${id})`);
    await assert.rejects(c.getAdapterAssurance(id), TypeError, `getAdapterAssurance(${id})`);
  }
  assert.equal(calls(), 0);
  // Identifiers that merely contain dots are encoded and sent as one segment.
  const seen: string[] = [];
  const c2 = new SteelmatterClient({ baseUrl: 'https://api.example', token: 't', fetch: (async (u: string | URL | Request) => { seen.push(String(u)); return new Response('{}', { status: 200, headers: { 'content-type': 'application/json' } }); }) as typeof fetch });
  await c2.getOperation('...');
  await c2.getOperation('op.1');
  await c2.getOperation('../x');
  assert.deepEqual(seen, ['https://api.example/v1/operations/...', 'https://api.example/v1/operations/op.1', 'https://api.example/v1/operations/..%2Fx']);
});

type Cancel = 'throws' | 'rejects' | 'never settles';
const cancelImpl = (how: Cancel) => (): Promise<void> => {
  if (how === 'throws') throw new Error('cancel failed');
  if (how === 'rejects') return Promise.reject(new Error('cancel failed'));
  return new Promise<void>(() => {});
};

/** A Response-shaped object from a custom fetch whose body refuses to cancel. */
function stubborn(how: Cancel, declared: boolean): () => Response {
  return () => {
    let sent = 0;
    const reader = {
      read: async () => {
        if (sent > LIMIT + 4 * MiB) return { done: true as const, value: undefined };
        sent += MiB;
        return { done: false as const, value: new Uint8Array(MiB).fill(0x20) };
      },
      cancel: cancelImpl(how),
      releaseLock: () => { throw new Error('releaseLock failed'); },
    };
    const body = { cancel: cancelImpl(how), getReader: () => reader };
    const headers = new Headers({ 'content-type': 'application/json', ...(declared ? { 'content-length': String(LIMIT + 1) } : {}) });
    return { type: 'basic', status: 200, ok: true, headers, body } as unknown as Response;
  };
}

test('custom-fetch cancellation failure on an oversized response is a SteelmatterResponseError with no retry', async () => {
  for (const declared of [true, false]) {
    for (const how of ['throws', 'rejects', 'never settles'] as Cancel[]) {
      const what = `${declared ? 'declared' : 'streamed'} size, cancel ${how}`;
      const { c, calls } = clientWith(stubborn(how, declared));
      const err = await c.getOperation('op-1').then(() => assert.fail(`${what}: resolved`), (e: unknown) => e);
      assert.ok(err instanceof SteelmatterResponseError, `${what}: got ${(err as Error)?.constructor?.name}: ${(err as Error)?.message}`);
      assert.match((err as Error).message, /larger than 16777216 bytes/, what);
      assert.equal(calls(), 1, `${what}: not retried`);
    }
  }
});
