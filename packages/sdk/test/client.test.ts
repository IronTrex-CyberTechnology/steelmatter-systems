// Unit tests: the client against a scripted fetch. No network, no server.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  SteelmatterClient, SteelmatterApiError, SteelmatterTransportError, SteelmatterResponseError, SteelmatterError,
  isPermitIssued, isDenied, amountMinor, formatINR, type ClientOptions, type ActionPayload,
} from '../src/index.ts';

interface Seen { method: string; url: string; headers: Record<string, string>; body: string | undefined; redirect: string | undefined }
type Reply = { status: number; body?: unknown; headers?: Record<string, string>; raw?: string } | Error | 'hang';

function fake(replies: Reply[]) {
  const seen: Seen[] = [];
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const headers: Record<string, string> = {};
    new Headers(init?.headers).forEach((v, k) => { headers[k] = v; });
    seen.push({ method: init?.method ?? 'GET', url: String(input), headers, body: init?.body as string | undefined, redirect: init?.redirect });
    const r = replies.shift();
    if (r === undefined) throw new Error('no scripted reply');
    if (r === 'hang') {
      return new Promise<Response>((_, reject) => init?.signal?.addEventListener('abort', () => reject(init.signal?.reason), { once: true }));
    }
    if (r instanceof Error) throw r;
    const text = r.raw ?? (r.body === undefined ? '' : JSON.stringify(r.body));
    return new Response(text === '' ? null : text, {
      status: r.status,
      headers: { 'content-type': 'application/json', 'x-request-id': 'req_' + '0'.repeat(24), ...r.headers },
    });
  }) as typeof fetch;
  return { seen, fetchImpl };
}

const sleeps: number[] = [];
function client(replies: Reply[], extra: Partial<ClientOptions> = {}) {
  const f = fake(replies);
  sleeps.length = 0;
  const c = new SteelmatterClient({
    baseUrl: 'http://127.0.0.1:8080/', token: 'tok-secret', fetch: f.fetchImpl,
    sleep: async (ms) => { sleeps.push(ms); }, random: () => 0.5, ...extra,
  });
  return { c, seen: f.seen };
}

const errorBody = (code: string, retryable = false, extra: Record<string, unknown> = {}) =>
  ({ error_code: code, message: `${code} message`, request_id: 'req_' + 'a'.repeat(24), retryable, ...extra });

const action: ActionPayload = {
  schema_version: '1', crypto_profile: 'SM-JWS-ES256-1', tenant_id: 't', instruction_id: 'ins-1', action_revision: 1,
  actor_id: 'agent-a', audience: 'synthetic-procurement', operation: 'purchase_order.issue', supplier_id: 'supplier-a',
  supplier_revision: 1, delivery_id: 'warehouse-a', currency: 'INR', lines: [{ item_id: 'item-a', quantity: 3, unit_price_minor: 20_000_000 }],
  amount_minor: 60_000_000,
};

test('every operation sends the documented method, path, body and headers', async () => {
  const ok = (status: number) => ({ status, body: { ok: true } });
  const { c, seen } = client(Array.from({ length: 11 }, (_, i) => ok([0, 2, 3, 4, 6].includes(i) ? 201 : 200)));
  const proposal = { supplier_id: 'supplier-a', delivery_id: 'warehouse-a', currency: 'INR' as const, lines: action.lines };
  await c.createMandate({ actor_ids: ['agent-a'], audience: 'synthetic-procurement', supplier_ids: ['supplier-a'], delivery_ids: ['warehouse-a'], currency: 'INR', limit_minor: 100_000_000, expires_at: '2026-10-16T12:00:00Z' });
  await c.revokeMandate('m-1');
  await c.createInstruction({ source_system: 'erp', source_record_id: 'po-1', mandate_id: 'm-1', proposed_action: proposal });
  await c.amendInstruction('ins-1', { amendment_id: 'am-1', expected_action_revision: 1, proposed_action: proposal });
  await c.evaluateAction({ instruction_id: 'ins-1', action_revision: 1 });
  await c.simulateAction({ instruction_id: 'ins-1', action_revision: 1 });
  await c.admitOperation({ action, permit_jws: 'a.b.c' });
  await c.getOperation('op-1');
  await c.reconcileOperation('op-1');
  await c.getEvidence('op-1');
  await c.getAdapterAssurance('synthetic-procurement');
  assert.deepEqual(seen.map((s) => `${s.method} ${s.url.replace('http://127.0.0.1:8080', '')}`), [
    'POST /v1/mandates', 'POST /v1/mandates/m-1/revoke', 'POST /v1/instructions', 'POST /v1/instructions/ins-1/revisions',
    'POST /v1/actions/evaluate', 'POST /v1/actions/simulate', 'POST /v1/operations', 'GET /v1/operations/op-1',
    'POST /v1/operations/op-1/reconcile', 'GET /v1/operations/op-1/evidence', 'GET /v1/adapters/synthetic-procurement/assurance',
  ]);
  for (const s of seen) {
    assert.equal(s.headers.authorization, 'Bearer tok-secret');
    assert.equal(s.headers.accept, 'application/json');
    assert.equal(s.redirect, 'manual', 'redirects are handed back and refused, never followed');
    if (s.body === undefined) assert.equal(s.headers['content-type'], undefined);
    else assert.equal(s.headers['content-type'], 'application/json');
  }
  assert.equal(seen[1]!.body, undefined, 'revoke sends no body');
  assert.equal(seen[8]!.body, undefined, 'reconcile sends no body');
  assert.deepEqual(JSON.parse(seen[6]!.body!), { action, permit_jws: 'a.b.c' });
  assert.deepEqual(JSON.parse(seen[4]!.body!), { instruction_id: 'ins-1', action_revision: 1 });
});

test('201 reports created, 200 reports an existing result', async () => {
  const { c } = client([{ status: 201, body: { instruction_id: 'i' } }, { status: 200, body: { instruction_id: 'i' } }]);
  const first = await c.createInstruction({ source_system: 's', source_record_id: 'r', mandate_id: 'm', proposed_action: { supplier_id: 'x', delivery_id: 'd', currency: 'INR', lines: [] } });
  assert.equal(first.created, true);
  assert.equal(first.status, 201);
  assert.equal(first.requestId, 'req_' + '0'.repeat(24));
  assert.equal(first.attempts, 1);
  const again = await c.createInstruction({ source_system: 's', source_record_id: 'r', mandate_id: 'm', proposed_action: { supplier_id: 'x', delivery_id: 'd', currency: 'INR', lines: [] } });
  assert.equal(again.created, false);
});

test('path identifiers are percent-encoded', async () => {
  const { c, seen } = client([{ status: 200, body: {} }]);
  await c.getOperation('op/../x?y');
  assert.equal(seen[0]!.url, 'http://127.0.0.1:8080/v1/operations/op%2F..%2Fx%3Fy');
  await assert.rejects(() => c.getOperation(''), TypeError);
});

test('error envelope becomes SteelmatterApiError with every field', async () => {
  const { c, seen } = client([{ status: 409, body: errorBody('ACTION_CHANGED', false, { operation_id: 'op-9' }) }]);
  const err = await c.admitOperation({ action, permit_jws: 'a.b.c' }).catch((e) => e);
  assert.ok(err instanceof SteelmatterApiError && err instanceof SteelmatterError);
  assert.equal(err.status, 409);
  assert.equal(err.code, 'ACTION_CHANGED');
  assert.equal(err.retryable, false);
  assert.equal(err.operationId, 'op-9');
  assert.equal(err.requestId, 'req_' + '0'.repeat(24));
  assert.match(err.message, /ACTION_CHANGED \(HTTP 409\)/);
  assert.doesNotMatch(err.message, /tok-secret/);
  assert.equal(seen.length, 1, 'non-retryable errors are not retried');
});

test('evaluate DENY is a successful response, not an error', async () => {
  const { c } = client([{ status: 200, body: { decision: 'DENY', reason: 'CAPACITY_EXHAUSTED', message: 'm', instruction_id: 'i', action_revision: 1 } }]);
  const r = await c.evaluateAction({ instruction_id: 'i', action_revision: 1 });
  assert.ok(!isPermitIssued(r));
  assert.ok(isDenied(r));
  assert.equal(r.data.reason, 'CAPACITY_EXHAUSTED'); // narrowed by the guard
  assert.ok(isDenied(r.data) && !isPermitIssued(r.data));
});

test('idempotent calls retry 503 with backoff and keep the identical body', async () => {
  const { c, seen } = client([
    { status: 503, body: errorBody('SERVICE_UNAVAILABLE', true) },
    { status: 503, body: errorBody('RETRIES_EXHAUSTED', true, { operation_id: 'op-1' }) },
    { status: 201, body: { operation_id: 'op-1' } },
  ]);
  const r = await c.admitOperation({ action, permit_jws: 'a.b.c' });
  assert.equal(r.attempts, 3);
  assert.equal(r.created, true);
  assert.deepEqual(sleeps, [75, 150]); // base 100, doubling, jitter 0.5 → 50%+25%
  assert.equal(new Set(seen.map((s) => s.body)).size, 1, 'retries resend the identical request');
});

test('retries stop at maxAttempts and surface the last error', async () => {
  const { c, seen } = client([503, 503, 503].map((s) => ({ status: s, body: errorBody('SERVICE_UNAVAILABLE', true) })));
  const err = await c.getOperation('op-1').catch((e) => e);
  assert.ok(err instanceof SteelmatterApiError);
  assert.equal(err.attempts, 3);
  assert.equal(seen.length, 3);
});

test('429 honours Retry-After, also for mandate creation', async () => {
  const { c, seen } = client([
    { status: 429, body: errorBody('RATE_LIMITED', true), headers: { 'retry-after': '2' } },
    { status: 201, body: { mandate_digest: 'd' } },
  ]);
  const r = await c.createMandate({ actor_ids: ['a'], audience: 'x', supplier_ids: ['s'], delivery_ids: ['d'], currency: 'INR', limit_minor: 1, expires_at: '2026-10-16T12:00:00Z' });
  assert.equal(r.created, true);
  assert.deepEqual(sleeps, [2000]);
  assert.equal(seen.length, 2);
});

test('429 with a Retry-After beyond maxRetryAfterMs is not retried', async () => {
  const { c, seen } = client([{ status: 429, body: errorBody('RATE_LIMITED', true), headers: { 'retry-after': '120' } }]);
  const err = await c.getOperation('op').catch((e) => e);
  assert.ok(err instanceof SteelmatterApiError);
  assert.equal(err.retryAfterMs, 120_000);
  assert.equal(seen.length, 1);
});

test('mandate creation is never retried after 503 or a transport failure (not idempotent)', async () => {
  const body = { actor_ids: ['a'], audience: 'x', supplier_ids: ['s'], delivery_ids: ['d'], currency: 'INR' as const, limit_minor: 1, expires_at: '2026-10-16T12:00:00Z' };
  let { c, seen } = client([{ status: 503, body: errorBody('SERVICE_UNAVAILABLE', true) }]);
  assert.ok((await c.createMandate(body).catch((e) => e)) instanceof SteelmatterApiError);
  assert.equal(seen.length, 1);
  ({ c, seen } = client([new TypeError('fetch failed')]));
  const err = await c.createMandate(body).catch((e) => e);
  assert.ok(err instanceof SteelmatterTransportError && !err.timedOut);
  assert.equal(seen.length, 1);
});

test('transport failures are retried for idempotent calls', async () => {
  const { c, seen } = client([new TypeError('fetch failed'), { status: 200, body: { operation_id: 'op' } }]);
  const r = await c.getOperation('op');
  assert.equal(r.attempts, 2);
  assert.equal(seen.length, 2);
});

test('per-attempt timeout produces a timed-out transport error', async () => {
  const { c } = client(['hang', 'hang'], { timeoutMs: 20, retry: { maxAttempts: 2 } });
  const err = await c.getEvidence('op').catch((e) => e);
  assert.ok(err instanceof SteelmatterTransportError);
  assert.equal(err.timedOut, true);
  assert.equal(err.attempts, 2);
});

test('caller abort cancels immediately without retrying', async () => {
  const { c, seen } = client(['hang', 'hang']);
  const ctl = new AbortController();
  const p = c.getOperation('op', { signal: ctl.signal });
  setTimeout(() => ctl.abort(new Error('caller gave up')), 10);
  await assert.rejects(p, /caller gave up/);
  assert.equal(seen.length, 1);
  await assert.rejects(c.getOperation('op', { signal: AbortSignal.abort(new Error('already')) }), /already/);
});

test('retry: false and per-call options disable retries', async () => {
  let { c, seen } = client([{ status: 503, body: errorBody('SERVICE_UNAVAILABLE', true) }], { retry: false });
  await assert.rejects(c.getOperation('op'), SteelmatterApiError);
  assert.equal(seen.length, 1);
  ({ c, seen } = client([{ status: 503, body: errorBody('SERVICE_UNAVAILABLE', true) }]));
  await assert.rejects(c.getOperation('op', { retry: false }), SteelmatterApiError);
  assert.equal(seen.length, 1);
});

test('responses outside the contract raise SteelmatterResponseError and are not retried', async () => {
  const cases: Reply[] = [
    { status: 200, raw: '<html>', headers: { 'content-type': 'text/html' } },
    { status: 200, raw: '{not json' },
    { status: 200, raw: '[1,2]' },
    { status: 204 },
    { status: 418, body: { teapot: true } },
    { status: 302, body: {} },
  ];
  for (const reply of cases) {
    const { c, seen } = client([reply]);
    const err = await c.getOperation('op').catch((e) => e);
    assert.ok(err instanceof SteelmatterResponseError, `${JSON.stringify(reply)} → ${err}`);
    assert.equal(seen.length, 1);
  }
});

test('the token can be a function and is read per attempt', async () => {
  let n = 0;
  const { c, seen } = client([{ status: 503, body: errorBody('SERVICE_UNAVAILABLE', true) }, { status: 200, body: {} }], { token: async () => `tok-${++n}` });
  await c.getOperation('op');
  assert.deepEqual(seen.map((s) => s.headers.authorization), ['Bearer tok-1', 'Bearer tok-2']);
  const bad = client([], { token: () => 'a\r\nX-Evil: 1' });
  await assert.rejects(bad.c.getOperation('op'), /invalid token/);
});

test('constructor validates its options', () => {
  const f = fake([]).fetchImpl;
  assert.throws(() => new SteelmatterClient({ baseUrl: 'not a url', token: 't', fetch: f }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'ftp://x', token: 't', fetch: f }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'https://u:p@x', token: 't', fetch: f }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'https://x', token: '', fetch: f }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'https://x', token: 't', fetch: f, retry: { maxAttempts: 0 } }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'https://x', token: 't', fetch: f, timeoutMs: 0 }), TypeError);
  assert.throws(() => new SteelmatterClient({ baseUrl: 'https://x', token: 't', fetch: f, headers: { Authorization: 'x' } }), TypeError);
  assert.equal(new SteelmatterClient({ baseUrl: 'https://x:1/api/', token: 't', fetch: f }).baseUrl, 'https://x:1/api');
});

test('custom headers are sent', async () => {
  const { c, seen } = client([{ status: 200, body: {} }], { headers: { 'X-Trace': 'abc' } });
  await c.getOperation('op');
  assert.equal(seen[0]!.headers['x-trace'], 'abc');
});

test('amountMinor uses checked integer arithmetic', () => {
  assert.equal(amountMinor(action.lines), 60_000_000);
  assert.equal(amountMinor([{ item_id: 'a', quantity: 1, unit_price_minor: Number.MAX_SAFE_INTEGER }]), Number.MAX_SAFE_INTEGER);
  assert.throws(() => amountMinor([{ item_id: 'a', quantity: 2, unit_price_minor: Number.MAX_SAFE_INTEGER }]), RangeError);
  assert.throws(() => amountMinor([{ item_id: 'a', quantity: 1.5, unit_price_minor: 1 }]), RangeError);
  assert.throws(() => amountMinor([{ item_id: 'a', quantity: 0, unit_price_minor: 1 }]), RangeError);
});

test('formatINR uses Indian grouping on integer paise', () => {
  assert.equal(formatINR(100_000_000), 'INR 10,00,000');
  assert.equal(formatINR(60_000_000), 'INR 6,00,000');
  assert.equal(formatINR(0), 'INR 0');
  assert.equal(formatINR(123_450), 'INR 1,234.50');
  assert.equal(formatINR(1_000_000_000), 'INR 1,00,00,000');
  assert.throws(() => formatINR(1.5), RangeError);
});
