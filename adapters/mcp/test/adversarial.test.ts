import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { McpServer, type ActorClient, type JsonRpcResponse } from '../src/index.ts';
import { MAX_BATCH_SIZE, MAX_IN_FLIGHT_CALLS } from '../src/server.ts';

const init = (id: string, protocolVersion = '2025-11-25') => ({jsonrpc: '2.0', id, method: 'initialize', params: {protocolVersion, capabilities: {}, clientInfo: {name: 'synthetic', version: '1'}}});
const request = (id: string | number) => ({jsonrpc: '2.0', id, method: 'tools/call', params: {name: 'steelmatter_get_operation', arguments: {operation_id: 'op'}}});
function harness() {
  const signals: AbortSignal[] = [];
  const finish: (() => void)[] = [];
  const client = {getOperation: async (_: string, opts: {signal: AbortSignal}) => {
    signals.push(opts.signal);
    return new Promise((resolve, reject) => {
      finish.push(() => resolve({status: 200, created: false, requestId: 'synthetic', attempts: 1, data: {}}));
      opts.signal.addEventListener('abort', () => reject(opts.signal.reason), {once: true});
    });
  }} as unknown as ActorClient;
  return {server: new McpServer({client}), signals, finish};
}

test('adversarial MCP: reinitialization cannot downgrade a running session to batching', async () => {
  const {server, signals} = harness();
  await server.handle(init('init'));
  const again = await server.handle(init('again', '2025-03-26')) as JsonRpcResponse;
  assert.equal(again.error?.code, -32600);
  const batch = await server.handle([request('batch')]) as JsonRpcResponse;
  assert.equal(batch.error?.code, -32600);
  assert.equal(signals.length, 0);
});

test('adversarial MCP: duplicate active IDs cannot replace the original cancellation target', async () => {
  const {server, signals, finish} = harness();
  await server.handle(init('init'));
  const first = server.handle(request('same'));
  let second: Promise<unknown> | undefined;
  try {
    second = server.handle(request('same'));
    assert.equal(signals.length, 1, 'second request must never reach the SDK');
    assert.equal((await second as JsonRpcResponse).error?.code, -32600);
    await server.handle({jsonrpc: '2.0', method: 'notifications/cancelled', params: {requestId: 'same'}});
    assert.equal(signals[0]!.aborted, true);
    assert.equal(await first, undefined);
  } finally {
    finish.forEach(f => f());
    await Promise.all([first, second]);
  }
});

test('adversarial MCP: fractional, non-finite and unsafe numeric IDs never execute', async () => {
  const {server, signals, finish} = harness();
  await server.handle(init('init'));
  for (const id of [1.5, Infinity, NaN, Number.MAX_SAFE_INTEGER + 1]) {
    const pending = server.handle(request(id));
    try {
      assert.equal(signals.length, 0, `invalid ID ${id} reached SDK`);
      assert.equal((await pending as JsonRpcResponse).error?.code, -32600);
    } finally { finish.forEach(f => f()); await pending; }
  }
});

test('adversarial MCP: unfinished oversized stdio lines are rejected before newline or EOF', {timeout: 10000}, async () => {
  const child = spawn(process.execPath, [fileURLToPath(new URL('../src/main.ts', import.meta.url))], {
    env: {...process.env, STEELMATTER_SERVER: 'http://127.0.0.1:1', STEELMATTER_MCP_TOKEN: 'synthetic-test-token', STEELMATTER_MCP_TOKEN_FILE: ''},
  });
  const closed = new Promise<void>(resolve => child.on('close', () => resolve()));
  let out = '';
  const output = new Promise<string>(resolve => child.stdout.on('data', chunk => { out += chunk; if (out.includes('\n')) resolve(out); }));
  const timeout = setTimeout(() => child.kill(), 5000);
  try {
    child.stdin.on('error', () => {});
    child.stdin.write('x'.repeat((16 << 20) + 1)); // bounded synthetic input; deliberately no newline
    const result = await Promise.race([output, closed.then(() => '')]);
    assert.notEqual(result, '', 'line limit was not enforced before newline/EOF');
    assert.equal(JSON.parse(result.trim()).error.code, -32600);
  } finally { clearTimeout(timeout); child.kill(); await closed; }
});

test('adversarial MCP: concurrent call limit rejects excess work and releases completed slots', async () => {
  const {server, signals, finish} = harness();
  await server.handle(init('init'));
  const calls = Array.from({length: MAX_IN_FLIGHT_CALLS}, (_, i) => server.handle(request(`work-${i}`)));
  try {
    const excess = await server.handle(request('excess')) as JsonRpcResponse;
    assert.equal(excess.error?.code, -32000);
    assert.equal(signals.length, MAX_IN_FLIGHT_CALLS);
    // Cancellation must still work when the session is saturated.
    await server.handle({jsonrpc: '2.0', method: 'notifications/cancelled', params: {requestId: 'work-0'}});
    assert.equal(await calls[0], undefined);
    const next = server.handle(request('replacement'));
    calls.push(next);
    assert.equal(signals.length, MAX_IN_FLIGHT_CALLS + 1);
    finish.at(-1)!();
    assert.equal((await next as JsonRpcResponse).error, undefined);
  } finally { finish.forEach(f => f()); await Promise.all(calls); }
});

test('adversarial MCP: oversized legacy batches execute no member; limit-sized batches still work', async () => {
  const {server, signals} = harness();
  await server.handle(init('init', '2025-03-26'));
  const rejected = await server.handle(Array.from({length: MAX_BATCH_SIZE + 1}, (_, i) => request(i))) as JsonRpcResponse;
  assert.equal(rejected.error?.code, -32600);
  assert.equal(signals.length, 0);
  const accepted = await server.handle(Array.from({length: MAX_BATCH_SIZE}, (_, i) => ({jsonrpc: '2.0', id: i, method: 'ping'}))) as JsonRpcResponse[];
  assert.equal(accepted.length, MAX_BATCH_SIZE);
  assert.ok(accepted.every(r => r.error === undefined));
});

test('adversarial MCP: cancellation suppresses a result even when the client completes after abort', async () => {
  let finish!: (value: unknown) => void;
  const client = {getOperation: () => new Promise(resolve => { finish = resolve; })} as unknown as ActorClient;
  const server = new McpServer({client});
  await server.handle(init('init'));
  const pending = server.handle(request('cancel'));
  await server.handle({jsonrpc: '2.0', method: 'notifications/cancelled', params: {requestId: 'cancel'}});
  finish({status: 200, created: false, requestId: 'synthetic', attempts: 1, data: {}});
  assert.equal(await pending, undefined);
});

test('adversarial MCP: EOF drains a large queued response before exiting', {timeout: 10000}, async () => {
  const child = spawn(process.execPath, [fileURLToPath(new URL('../src/main.ts', import.meta.url))], {
    env: {...process.env, STEELMATTER_SERVER: 'http://127.0.0.1:1', STEELMATTER_MCP_TOKEN: 'synthetic-test-token', STEELMATTER_MCP_TOKEN_FILE: ''},
  });
  const closed = new Promise<number | null>(resolve => child.on('close', resolve));
  const timeout = setTimeout(() => child.kill(), 5000);
  let out = '';
  child.stdout.on('data', chunk => { out += chunk; });
  child.stdout.pause();
  const resume = setTimeout(() => child.stdout.resume(), 250);
  const id = 'x'.repeat(512 << 10);
  try {
    child.stdin.end(JSON.stringify({jsonrpc: '2.0', id, method: 'ping'}) + '\n');
    assert.equal(await closed, 0);
    assert.equal(JSON.parse(out).id, id);
  } finally { clearTimeout(timeout); clearTimeout(resume); child.kill(); await closed; }
});
