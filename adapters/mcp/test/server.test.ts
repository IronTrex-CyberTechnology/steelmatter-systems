// Protocol and tool tests for the MCP server core, with a scripted SDK client.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SteelmatterApiError, SteelmatterTransportError, SteelmatterResponseError, type ApiResponse } from '@irontrex/steelmatter-sdk';
import { McpServer, SUPPORTED_PROTOCOL_VERSIONS, ErrorCodes, toolDefinitions, type ActorClient, type JsonRpcResponse } from '../src/index.ts';

type Call = { method: string; args: unknown[] };

function fakeClient(behaviour: (method: string, args: unknown[]) => Promise<unknown> | unknown) {
  const calls: Call[] = [];
  const make = (method: string) => async (...args: unknown[]) => {
    calls.push({ method, args });
    const out = await behaviour(method, args);
    return { status: method === 'admitOperation' ? 201 : 200, created: method === 'admitOperation', requestId: 'req_' + 'b'.repeat(24), attempts: 1, data: out } as ApiResponse<unknown>;
  };
  const client = {
    evaluateAction: make('evaluateAction'), admitOperation: make('admitOperation'), getOperation: make('getOperation'),
    getEvidence: make('getEvidence'), getAdapterAssurance: make('getAdapterAssurance'),
  } as unknown as ActorClient;
  return { client, calls };
}

async function ready(behaviour: Parameters<typeof fakeClient>[0] = () => ({ ok: true }), protocolVersion = '2025-06-18') {
  const { client, calls } = fakeClient(behaviour);
  const s = new McpServer({ client });
  const init = (await s.handle({ jsonrpc: '2.0', id: 0, method: 'initialize', params: { protocolVersion, capabilities: {}, clientInfo: { name: 't', version: '1' } } })) as JsonRpcResponse;
  assert.ok(init.result);
  await s.handle({ jsonrpc: '2.0', method: 'notifications/initialized' });
  return { s, calls };
}

const call = async (s: McpServer, name: string, args: unknown, id: number | string = 1) =>
  (await s.handle({ jsonrpc: '2.0', id, method: 'tools/call', params: { name, arguments: args } })) as JsonRpcResponse;

const action = { schema_version: '1', instruction_id: 'ins-1', supplier_id: 'supplier-a', amount_minor: 60000000 };

test('initialize negotiates a supported protocol version and declares only tools', async () => {
  const s = new McpServer({ client: fakeClient(() => ({})).client });
  for (const v of SUPPORTED_PROTOCOL_VERSIONS) {
    const r = (await s.handle({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: v } })) as JsonRpcResponse;
    assert.equal((r.result as { protocolVersion: string }).protocolVersion, v);
  }
  const r = (await s.handle({ jsonrpc: '2.0', id: 2, method: 'initialize', params: { protocolVersion: '1999-01-01' } })) as JsonRpcResponse;
  const res = r.result as { protocolVersion: string; capabilities: Record<string, unknown>; serverInfo: { name: string }; instructions: string };
  assert.equal(res.protocolVersion, SUPPORTED_PROTOCOL_VERSIONS[0]);
  assert.deepEqual(res.capabilities, { tools: { listChanged: false } });
  assert.equal(res.serverInfo.name, 'steelmatter-mcp');
  assert.match(res.instructions, /cannot change what is permitted/);
});

test('tools are refused before initialize; ping always works', async () => {
  const s = new McpServer({ client: fakeClient(() => ({})).client });
  const r = (await s.handle({ jsonrpc: '2.0', id: 1, method: 'tools/list' })) as JsonRpcResponse;
  assert.equal(r.error?.code, ErrorCodes.notInitialized);
  assert.deepEqual(((await s.handle({ jsonrpc: '2.0', id: 2, method: 'ping' })) as JsonRpcResponse).result, {});
});

test('the tool set is exactly the actor operations; nothing grants authority', async () => {
  const { s } = await ready();
  const r = (await s.handle({ jsonrpc: '2.0', id: 1, method: 'tools/list' })) as JsonRpcResponse;
  const tools = (r.result as { tools: { name: string; inputSchema: { additionalProperties: boolean; properties: Record<string, unknown> } }[] }).tools;
  assert.deepEqual(tools.map((t) => t.name).sort(), [
    'steelmatter_admit_operation', 'steelmatter_evaluate_action', 'steelmatter_get_adapter_assurance', 'steelmatter_get_evidence', 'steelmatter_get_operation',
  ]);
  for (const t of tools) {
    assert.equal(t.inputSchema.additionalProperties, false, `${t.name} schema must be closed`);
    for (const forbidden of ['actor_id', 'tenant_id', 'audience', 'destination', 'url', 'token', 'mandate_id', 'limit_minor']) {
      assert.ok(!(forbidden in t.inputSchema.properties), `${t.name} must not accept ${forbidden}`);
    }
    assert.doesNotMatch(t.name, /mandate|instruction|amend|revoke|simulate|reconcile/);
  }
  assert.deepEqual(toolDefinitions().map((t) => t.name).sort(), tools.map((t) => t.name).sort());
});

test('each tool calls exactly its SDK method with the given arguments', async () => {
  const { s, calls } = await ready();
  await call(s, 'steelmatter_evaluate_action', { instruction_id: 'ins-1', action_revision: 2 });
  await call(s, 'steelmatter_admit_operation', { action, permit_jws: 'aaa.bbb.ccc' });
  await call(s, 'steelmatter_get_operation', { operation_id: 'op-1' });
  await call(s, 'steelmatter_get_evidence', { operation_id: 'op-1' });
  await call(s, 'steelmatter_get_adapter_assurance', { adapter_id: 'synthetic-procurement' });
  assert.deepEqual(calls.map((c) => [c.method, c.args[0]]), [
    ['evaluateAction', { instruction_id: 'ins-1', action_revision: 2 }],
    ['admitOperation', { action, permit_jws: 'aaa.bbb.ccc' }],
    ['getOperation', 'op-1'],
    ['getEvidence', 'op-1'],
    ['getAdapterAssurance', 'synthetic-procurement'],
  ]);
  for (const c of calls) assert.ok((c.args[1] as { signal: AbortSignal }).signal instanceof AbortSignal, 'each call is cancellable');
});

test('a successful call returns text and structured content with status and request ID', async () => {
  const { s } = await ready(() => ({ operation_id: 'op-1', execution: 'COMMITTED' }));
  const r = await call(s, 'steelmatter_admit_operation', { action, permit_jws: 'aaa.bbb.ccc' });
  const res = r.result as { isError: boolean; structuredContent: Record<string, unknown>; content: { type: string; text: string }[] };
  assert.equal(res.isError, false);
  assert.deepEqual(res.structuredContent, { status: 201, created: true, request_id: 'req_' + 'b'.repeat(24), result: { operation_id: 'op-1', execution: 'COMMITTED' } });
  assert.equal(res.content[0]!.type, 'text');
  assert.deepEqual(JSON.parse(res.content[0]!.text), res.structuredContent);
});

test('service errors become tool results with isError and the stable code', async () => {
  const { s } = await ready(() => {
    throw new SteelmatterApiError({ status: 409, code: 'ACTION_CHANGED', message: 'presented action differs', requestId: 'req_' + 'c'.repeat(24), retryable: false, attempts: 1 });
  });
  const r = await call(s, 'steelmatter_admit_operation', { action, permit_jws: 'aaa.bbb.ccc' });
  const res = r.result as { isError: boolean; structuredContent: Record<string, unknown> };
  assert.equal(r.error, undefined, 'a refused action is a tool result, not a protocol error');
  assert.equal(res.isError, true);
  assert.equal(res.structuredContent.error_kind, 'API');
  assert.equal(res.structuredContent.error_code, 'ACTION_CHANGED');
  assert.equal(res.structuredContent.status, 409);
  assert.equal(res.structuredContent.retryable, false);
});

test('transport and contract failures are reported as such', async () => {
  let { s } = await ready(() => { throw new SteelmatterTransportError('timed out', { timedOut: true, attempts: 3 }); });
  let res = (await call(s, 'steelmatter_get_operation', { operation_id: 'op' })).result as { isError: boolean; structuredContent: Record<string, unknown> };
  assert.equal(res.isError, true);
  assert.equal(res.structuredContent.error_kind, 'TRANSPORT');
  assert.equal(res.structuredContent.timed_out, true);
  ({ s } = await ready(() => { throw new SteelmatterResponseError('bad', { status: 418 }); }));
  res = (await call(s, 'steelmatter_get_operation', { operation_id: 'op' })).result as typeof res;
  assert.equal(res.structuredContent.error_kind, 'RESPONSE');
  ({ s } = await ready(() => { throw new Error('bug'); }));
  const r = await call(s, 'steelmatter_get_operation', { operation_id: 'op' });
  assert.equal(r.error?.code, ErrorCodes.internalError);
  assert.equal(r.error?.message, 'Internal error', 'internal details are not leaked');
});

test('unknown tools and malformed tools/call requests are protocol errors and never reach the service', async () => {
  const { s, calls } = await ready();
  const unknown = await call(s, 'steelmatter_create_mandate', {});
  assert.equal(unknown.error?.code, ErrorCodes.invalidParams);
  const noName = (await s.handle({ jsonrpc: '2.0', id: 9, method: 'tools/call', params: {} })) as JsonRpcResponse;
  assert.equal(noName.error?.code, ErrorCodes.invalidParams);
  assert.equal(calls.length, 0);
});

test('non-object arguments or params are malformed requests (-32602) under every protocol version, and never reach the service', async () => {
  for (const v of SUPPORTED_PROTOCOL_VERSIONS) {
    const { s, calls } = await ready(undefined, v);
    for (const bad of [[], ['x'], 'x', 7, true, false, null]) {
      const r = await call(s, 'steelmatter_get_operation', bad);
      assert.equal(r.result, undefined, `${v} arguments=${JSON.stringify(bad)}`);
      assert.equal(r.error?.code, ErrorCodes.invalidParams, `${v} arguments=${JSON.stringify(bad)}`);
    }
    for (const params of [[], 'x', 7, null]) {
      const r = (await s.handle({ jsonrpc: '2.0', id: 3, method: 'tools/call', params })) as JsonRpcResponse;
      assert.equal(r.error?.code, ErrorCodes.invalidParams, `${v} params=${JSON.stringify(params)}`);
    }
    // An absent arguments member is "no arguments": the tool reports its missing input as a tool error.
    const absent = (await s.handle({ jsonrpc: '2.0', id: 4, method: 'tools/call', params: { name: 'steelmatter_get_operation' } })) as JsonRpcResponse;
    assert.equal((absent.result as { isError: boolean }).isError, true, `${v} absent arguments`);
    assert.equal(calls.length, 0, v);
  }
});

test('invalid tool arguments are tool results with isError and never reach the service', async () => {
  const { s, calls } = await ready();
  const cases: [string, unknown][] = [
    ['steelmatter_evaluate_action', { instruction_id: 'ins-1' }],
    ['steelmatter_evaluate_action', { instruction_id: 'ins-1', action_revision: 1, actor_id: 'agent-b' }],
    ['steelmatter_evaluate_action', { instruction_id: 'ins-1', action_revision: 1.5 }],
    ['steelmatter_evaluate_action', { instruction_id: 'ins-1', action_revision: 0 }],
    ['steelmatter_evaluate_action', { instruction_id: '../x', action_revision: 1 }],
    ['steelmatter_admit_operation', { action: 'x', permit_jws: 'aaa.bbb.ccc' }],
    ['steelmatter_admit_operation', { action, permit_jws: 'not a jws' }],
    ['steelmatter_admit_operation', { action, permit_jws: 'aaa.bbb.ccc', destination: 'https://elsewhere' }],
  ];
  for (const [name, args] of cases) {
    const r = await call(s, name, args);
    const what = `${name} ${JSON.stringify(args)}`;
    assert.equal(r.error, undefined, what);
    const res = r.result as { isError: boolean; content: { type: string; text: string }[]; structuredContent: { error_kind: string; message: string } };
    assert.equal(res.isError, true, what);
    assert.equal(res.structuredContent.error_kind, 'INVALID_ARGUMENTS', what);
    assert.match(res.structuredContent.message, new RegExp(`^Invalid arguments for ${name}: `), what);
    assert.equal(res.content[0]?.type, 'text');
  }
  assert.equal(calls.length, 0);
});

test('undeclared arguments with inherited names such as __proto__ are rejected', async () => {
  const { s, calls } = await ready();
  // Raw JSON, as a client sends it: JSON.parse makes "__proto__" an own key.
  const lines = [
    '{"instruction_id":"ins-1","action_revision":1,"__proto__":{"x":1}}',
    '{"instruction_id":"ins-1","action_revision":1,"constructor":"x"}',
    '{"instruction_id":"ins-1","action_revision":1,"toString":"x"}',
    '{"instruction_id":"ins-1","action_revision":1,"hasOwnProperty":"x"}',
  ];
  for (const [i, argsJson] of lines.entries()) {
    const line = `{"jsonrpc":"2.0","id":${i + 1},"method":"tools/call","params":{"name":"steelmatter_evaluate_action","arguments":${argsJson}}}`;
    const r = JSON.parse((await s.handleLine(line))!) as JsonRpcResponse;
    const res = r.result as { isError: boolean; structuredContent: { message: string } };
    assert.equal(res.isError, true, argsJson);
    assert.match(res.structuredContent.message, /unknown argument/, argsJson);
  }
  // A missing argument is not satisfied by an inherited property either.
  const r = await call(s, 'steelmatter_get_operation', Object.create({ operation_id: 'op' }));
  assert.equal((r.result as { isError: boolean }).isError, true);
  assert.equal(calls.length, 0);
});

test('JSON-RPC batches are accepted only under MCP 2025-03-26', async () => {
  const batch = [{ jsonrpc: '2.0', id: 'a', method: 'ping' }, { jsonrpc: '2.0', method: 'notifications/initialized' }, { jsonrpc: '2.0', id: 'b', method: 'ping' }];
  // Before initialize: refused, and initialize inside a batch never runs.
  const fresh = new McpServer({ client: fakeClient(() => ({})).client });
  const pre = (await fresh.handle([{ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-03-26' } }])) as JsonRpcResponse;
  assert.equal(pre.error?.code, ErrorCodes.invalidRequest);
  assert.equal(((await fresh.handle({ jsonrpc: '2.0', id: 2, method: 'tools/list' })) as JsonRpcResponse).error?.code, ErrorCodes.notInitialized);
  for (const v of ['2025-11-25', '2025-06-18']) {
    const { s, calls } = await ready(undefined, v);
    const r = (await s.handle([...batch, { jsonrpc: '2.0', id: 'c', method: 'tools/call', params: { name: 'steelmatter_get_operation', arguments: { operation_id: 'op' } } }])) as JsonRpcResponse;
    assert.ok(!Array.isArray(r), v);
    assert.equal(r.id, null, v);
    assert.equal(r.error?.code, ErrorCodes.invalidRequest, v);
    assert.equal(calls.length, 0, `${v}: no batch member runs`);
  }
  const { s } = await ready(undefined, '2025-03-26');
  const replies = (await s.handle(batch)) as JsonRpcResponse[];
  assert.deepEqual(replies.map((r) => r.id), ['a', 'b']);
  assert.equal(((await s.handle([])) as JsonRpcResponse).error?.code, ErrorCodes.invalidRequest);
});

test('JSON-RPC framing: parse errors, invalid requests, unknown methods, notifications, responses and batches', async () => {
  const { s } = await ready();
  assert.equal(JSON.parse((await s.handleLine('{not json'))!).error.code, ErrorCodes.parseError);
  assert.equal(((await s.handle({ id: 1, method: 'ping' })) as JsonRpcResponse).error?.code, ErrorCodes.invalidRequest);
  assert.equal(((await s.handle({ jsonrpc: '2.0', id: null, method: 'ping' })) as JsonRpcResponse).error?.code, ErrorCodes.invalidRequest);
  assert.equal(((await s.handle({ jsonrpc: '2.0', id: 1, method: 'resources/list' })) as JsonRpcResponse).error?.code, ErrorCodes.methodNotFound);
  assert.equal(await s.handle({ jsonrpc: '2.0', method: 'notifications/whatever' }), undefined);
  assert.equal(await s.handle({ jsonrpc: '2.0', id: 5, result: {} }), undefined);
  // ready() negotiates 2025-06-18, which has no batching (see the batch test).
  assert.equal(((await s.handle([{ jsonrpc: '2.0', id: 'a', method: 'ping' }])) as JsonRpcResponse).error?.code, ErrorCodes.invalidRequest);
  assert.equal(((await s.handle([])) as JsonRpcResponse).error?.code, ErrorCodes.invalidRequest);
});

test('notifications/cancelled aborts the in-flight call and suppresses its response', async () => {
  let seenSignal: AbortSignal | undefined;
  const { s } = await ready((_m, args) => {
    seenSignal = (args[1] as { signal: AbortSignal }).signal;
    return new Promise((_, reject) => seenSignal!.addEventListener('abort', () => reject(seenSignal!.reason), { once: true }));
  });
  const pending = call(s, 'steelmatter_get_operation', { operation_id: 'op' }, 'req-7');
  await new Promise((r) => setTimeout(r, 5));
  await s.handle({ jsonrpc: '2.0', method: 'notifications/cancelled', params: { requestId: 'req-7', reason: 'user stopped' } });
  assert.equal(await pending, undefined);
  assert.equal(seenSignal?.aborted, true);
});

test('get_operation confirms success only for COMMITTED and explains every other execution value', async () => {
  for (const [execution, confirmed] of [['COMMITTED', true], ['PENDING', false], ['UNRESOLVED', false], ['HELD', false], ['MISMATCH', false], ['FENCED', false]] as const) {
    const { s } = await ready(() => ({ operation_id: 'op', execution }));
    const res = (await call(s, 'steelmatter_get_operation', { operation_id: 'op' })).result as { isError: boolean; structuredContent: { result: Record<string, unknown> } };
    assert.equal(res.isError, false);
    assert.equal(res.structuredContent.result.execution, execution);
    assert.equal(res.structuredContent.result.outcome_confirmed, confirmed, execution);
    assert.equal(typeof res.structuredContent.result.outcome_note, confirmed ? 'undefined' : 'string', execution);
  }
});
