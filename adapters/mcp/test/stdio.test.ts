// The adapter process over stdio, through the real SDK, against a stub HTTP
// server: framing, credentials, configuration failures and shutdown.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdtempSync, writeFileSync } from 'node:fs';
import { createServer, type IncomingMessage } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { loadConfig } from '../src/index.ts';

const main = fileURLToPath(new URL('../src/main.ts', import.meta.url));
const TOKEN = 'synthetic-actor-token-0123456789';

function tokenFile(mode: number, content = TOKEN + '\n') {
  const dir = mkdtempSync(join(tmpdir(), 'smmcp-'));
  const f = join(dir, 'token');
  writeFileSync(f, content);
  chmodSync(f, mode);
  return f;
}

async function stub() {
  const seen: { method: string; url: string; auth: string | undefined; body: string }[] = [];
  const server = createServer(async (req: IncomingMessage, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    seen.push({ method: req.method!, url: req.url!, auth: req.headers.authorization, body });
    res.setHeader('content-type', 'application/json');
    res.setHeader('x-request-id', 'req_' + 'd'.repeat(24));
    if (req.url === '/v1/adapters/synthetic-procurement/assurance') {
      res.end(JSON.stringify({ adapter_id: 'synthetic-procurement', version: 'stub', simulation: true, supported_operations: ['purchase_order.issue'], supported_paths: [], limitations: [], test_references: [] }));
    } else {
      res.statusCode = 409;
      res.end(JSON.stringify({ error_code: 'ACTION_CHANGED', message: 'presented action differs from the permitted revision', request_id: 'req_' + 'd'.repeat(24), retryable: false }));
    }
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${(server.address() as { port: number }).port}`, seen, close: () => server.close() };
}

test('the stdio adapter answers MCP requests through the SDK and keeps the token out of all output', async () => {
  const s = await stub();
  try {
    const child = spawn(process.execPath, [main], { env: { ...process.env, STEELMATTER_SERVER: s.url, STEELMATTER_MCP_TOKEN_FILE: tokenFile(0o600) } });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (d) => (stdout += d));
    child.stderr.on('data', (d) => (stderr += d));
    const messages = [
      { jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'test', version: '1' } } },
      { jsonrpc: '2.0', method: 'notifications/initialized' },
      { jsonrpc: '2.0', id: 2, method: 'tools/list' },
      { jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'steelmatter_get_adapter_assurance', arguments: { adapter_id: 'synthetic-procurement' } } },
      { jsonrpc: '2.0', id: 4, method: 'tools/call', params: { name: 'steelmatter_admit_operation', arguments: { action: { supplier_id: 'supplier-b' }, permit_jws: 'aaa.bbb.ccc' } } },
    ];
    child.stdin.write(messages.map((m) => JSON.stringify(m)).join('\n') + '\n');
    child.stdin.write('this is not json\n');
    child.stdin.end();
    const code = await new Promise<number | null>((r) => child.on('close', r));
    assert.equal(code, 0, stderr);

    const lines = stdout.trim().split('\n');
    const replies = lines.map((l) => JSON.parse(l)); // every stdout line is a JSON-RPC message
    const byId = new Map(replies.map((r) => [r.id, r]));
    assert.equal(byId.get(1).result.protocolVersion, '2025-06-18');
    assert.equal(byId.get(2).result.tools.length, 5);
    assert.equal(byId.get(3).result.isError, false);
    assert.equal(byId.get(3).result.structuredContent.result.adapter_id, 'synthetic-procurement');
    assert.equal(byId.get(4).result.isError, true);
    assert.equal(byId.get(4).result.structuredContent.error_code, 'ACTION_CHANGED');
    assert.equal(byId.get(null).error.code, -32700);
    assert.equal(replies.length, 5, 'the notification gets no reply');

    assert.ok(s.seen.every((r) => r.auth === `Bearer ${TOKEN}`), 'the configured token authenticates every request');
    assert.deepEqual(s.seen.map((r) => `${r.method} ${r.url}`), ['GET /v1/adapters/synthetic-procurement/assurance', 'POST /v1/operations']);
    assert.ok(!stdout.includes(TOKEN) && !stderr.includes(TOKEN), 'the token never appears in output');
    assert.match(stderr, /ready; service http:\/\/127\.0\.0\.1/);
  } finally {
    s.close();
  }
});

test('the adapter refuses to start without a safe credential', () => {
  const start = (env: Record<string, string>) => spawnSync(process.execPath, [main], { env: { PATH: process.env.PATH ?? '', ...env }, input: '', encoding: 'utf8' });
  const cases: [string, Record<string, string>, RegExp][] = [
    ['no token', {}, /is required/],
    ['group/world-readable token file', { STEELMATTER_MCP_TOKEN_FILE: tokenFile(0o644) }, /owner only/],
    ['empty token file', { STEELMATTER_MCP_TOKEN_FILE: tokenFile(0o600, '\n') }, /empty/],
    ['both sources', { STEELMATTER_MCP_TOKEN_FILE: tokenFile(0o600), STEELMATTER_MCP_TOKEN: TOKEN }, /only one/],
    ['bad timeout', { STEELMATTER_MCP_TOKEN: TOKEN, STEELMATTER_MCP_TIMEOUT_MS: 'soon' }, /positive integer/],
    ['bad server URL', { STEELMATTER_MCP_TOKEN: TOKEN, STEELMATTER_SERVER: 'ftp://x' }, /http or https/],
  ];
  for (const [name, env, msg] of cases) {
    const r = start(env);
    assert.equal(r.status, 2, `${name}: ${r.stderr}`);
    assert.match(r.stderr, msg, name);
    assert.equal(r.stdout, '', `${name}: nothing on stdout`);
  }
});

test('loadConfig reads the documented variables', () => {
  assert.deepEqual(loadConfig({ STEELMATTER_MCP_TOKEN: ` ${TOKEN} ` }), { baseUrl: 'http://127.0.0.1:8080', token: TOKEN });
  assert.deepEqual(loadConfig({ STEELMATTER_MCP_TOKEN_FILE: tokenFile(0o600), STEELMATTER_SERVER: 'http://h:1', STEELMATTER_MCP_TIMEOUT_MS: '5000' }),
    { baseUrl: 'http://h:1', token: TOKEN, timeoutMs: 5000 });
  assert.throws(() => loadConfig({ STEELMATTER_MCP_TOKEN: 'two words' }), /whitespace/);
});
