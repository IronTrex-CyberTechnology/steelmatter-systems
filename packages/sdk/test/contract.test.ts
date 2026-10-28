// Contract tests: the SDK surface matches api/openapi.yaml. A new or renamed
// operation, a changed success status or a new error/deny code fails here.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { SteelmatterApiError, SteelmatterClient, SteelmatterResponseError, ERROR_CODES, ERROR_STATUS, DENY_REASONS } from '../src/index.ts';

const spec = readFileSync(new URL('../../../api/openapi.yaml', import.meta.url), 'utf8').split('\n');

interface Op { method: string; path: string; id: string; success: number[]; errors: number[] }

// A narrow reader for this document's fixed layout (two-space indentation).
function operations(): Op[] {
  const ops: Op[] = [];
  let path = '';
  let cur: Op | undefined;
  let inPaths = false;
  for (const line of spec) {
    if (line === 'paths:') { inPaths = true; continue; }
    if (inPaths && /^\S/.test(line)) break;
    let m = /^ {2}(\/v1\/\S+):$/.exec(line);
    if (m) { path = m[1]!; continue; }
    m = /^ {4}(get|post):$/.exec(line);
    if (m) { cur = { method: m[1]!.toUpperCase(), path, id: '', success: [], errors: [] }; ops.push(cur); continue; }
    m = /^ {6}operationId: (\w+)$/.exec(line);
    if (m && cur) { cur.id = m[1]!; continue; }
    m = /^ {8}'(2\d\d)':/.exec(line);
    if (m && cur) { cur.success.push(Number(m[1])); continue; }
    m = /^ {8}'([45]\d\d)':/.exec(line);
    if (m && cur) cur.errors.push(Number(m[1]));
  }
  return ops;
}

function enumOf(name: string): string[] {
  const start = spec.indexOf(`    ${name}:`);
  assert.ok(start >= 0, `schema ${name} not found`);
  const out: string[] = [];
  let inEnum = false;
  for (const line of spec.slice(start + 1)) {
    if (/^ {4}\S/.test(line)) break;
    if (/^ {6}enum:$/.test(line)) { inEnum = true; continue; }
    const m = /^ {8}- (\w+)$/.exec(line);
    if (inEnum && m) out.push(m[1]!);
    else if (inEnum) break;
  }
  return out;
}

const ops = operations();

const isR1 = (o: Op) => o.path.startsWith('/v1/dispatches') || o.path.startsWith('/v1/recovery');

test('the OpenAPI document declares the 11 documented operations', () => {
  assert.equal(ops.filter((o) => !isR1(o)).length, 11);
});

test('the OpenAPI document declares the 12 R1 dispatch, repair and recovery operations', () => {
  assert.equal(ops.filter(isR1).length, 12);
});

// POST operations that take no request body.
const noBody = ['revokeMandate', 'reconcileOperation', 'settleDispatch', 'confirmRepair', 'checkRestore', 'reconcileRestore', 'resumeWrites'];

test('every OpenAPI operationId is a client method, and nothing else is', () => {
  const methods = Object.getOwnPropertyNames(SteelmatterClient.prototype).filter((n) => n !== 'constructor').sort();
  assert.deepEqual(methods, ops.map((o) => o.id).sort());
});

test('each method calls its operation with the documented method, path and success statuses', async () => {
  const sample: Record<string, string> = { mandate_id: 'm-1', instruction_id: 'i-1', operation_id: 'o-1', adapter_id: 'a-1', op_ref: 'op-1', repair_id: 'rep-1' };
  for (const op of ops) {
    const seen: { method: string; path: string }[] = [];
    for (const status of op.success) {
      const client = new SteelmatterClient({
        baseUrl: 'http://127.0.0.1:8080', token: 't', retry: false,
        fetch: (async (url: string, init: RequestInit) => {
          seen.push({ method: init.method!, path: new URL(url).pathname });
          return new Response('{}', { status, headers: { 'content-type': 'application/json' } });
        }) as typeof fetch,
      });
      const params = [...op.path.matchAll(/\{(\w+)\}/g)].map((m) => sample[m[1]!]!);
      const fn = (client as unknown as Record<string, (...a: unknown[]) => Promise<{ status: number; created: boolean }>>)[op.id]!;
      const args: unknown[] = [...params];
      if (!noBody.includes(op.id) && op.method === 'POST') args.push({});
      const r = await fn.apply(client, args);
      assert.equal(r.status, status, `${op.id} must accept documented ${status}`);
      assert.equal(r.created, status === 201);
    }
    const expectedPath = op.path.replace(/\{(\w+)\}/g, (_, k: string) => sample[k]!);
    assert.ok(seen.length > 0 && seen.every((s) => s.method === op.method && s.path === expectedPath), `${op.id}: ${JSON.stringify(seen)}`);
  }
});

test('ErrorCode and DenyReason enumerations equal the OpenAPI document', () => {
  assert.deepEqual([...ERROR_CODES].sort(), enumOf('ErrorCode').sort());
  assert.deepEqual([...DENY_REASONS].sort(), enumOf('DenyReason').sort());
});

test('ERROR_STATUS equals core/httpapi/codes.go: each error code has exactly its one status', () => {
  const go = readFileSync(new URL('../../../core/httpapi/codes.go', import.meta.url), 'utf8');
  const statusOf: Record<string, number> = {
    StatusBadRequest: 400, StatusUnauthorized: 401, StatusForbidden: 403, StatusNotFound: 404, StatusConflict: 409,
    StatusUnprocessableEntity: 422, StatusTooManyRequests: 429, StatusInternalServerError: 500, StatusServiceUnavailable: 503,
  };
  const fromGo: Record<string, number> = {};
  for (const m of go.matchAll(/\{"(\w+)", http\.(Status\w+), (?:true|false), (Use\w+),/g)) {
    if (m[3] === 'UseDeny') continue;
    assert.ok(m[2]! in statusOf, `unmapped ${m[2]}`);
    fromGo[m[1]!] = statusOf[m[2]!]!;
  }
  assert.deepEqual({ ...ERROR_STATUS }, fromGo);
});

test('each operation types exactly its documented error statuses as SteelmatterApiError', async () => {
  const codeFor: Record<number, string> = {
    400: 'SCHEMA_VIOLATION', 401: 'UNAUTHENTICATED', 403: 'FORBIDDEN', 404: 'NOT_FOUND', 409: 'REVISION_STALE',
    422: 'UNSUPPORTED_VERSION', 429: 'RATE_LIMITED', 500: 'INTERNAL_ERROR', 503: 'SERVICE_UNAVAILABLE',
  };
  const sample: Record<string, string> = { mandate_id: 'm-1', instruction_id: 'i-1', operation_id: 'o-1', adapter_id: 'a-1', op_ref: 'op-1', repair_id: 'rep-1' };
  for (const op of ops) {
    assert.ok(op.errors.length > 0, `${op.id} documents error statuses`);
    for (const [s, code] of Object.entries(codeFor)) {
      const status = Number(s);
      const client = new SteelmatterClient({
        baseUrl: 'http://127.0.0.1:8080', token: 't', retry: false,
        fetch: (async () => new Response(JSON.stringify({ error_code: code, message: 'm', request_id: 'req_' + 'a'.repeat(24), retryable: status === 429 || status === 503 }),
          { status, headers: { 'content-type': 'application/json' } })) as typeof fetch,
      });
      const params = [...op.path.matchAll(/\{(\w+)\}/g)].map((m) => sample[m[1]!]!);
      const args: unknown[] = [...params];
      if (!noBody.includes(op.id) && op.method === 'POST') args.push({});
      const fn = (client as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>)[op.id]!;
      const err = await fn.apply(client, args).then(() => undefined, (e: unknown) => e);
      if (op.errors.includes(status)) assert.ok(err instanceof SteelmatterApiError, `${op.id} ${status} ${code} documented: ApiError`);
      else assert.ok(err instanceof SteelmatterResponseError, `${op.id} ${status} ${code} undocumented: ResponseError`);
    }
  }
});
