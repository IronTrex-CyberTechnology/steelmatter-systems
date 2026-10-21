// Installed-package check: build, pack and install the SDK into an empty
// project, then use it exactly as an external developer would: import by
// name, typecheck a TypeScript consumer against the shipped declarations, and
// call a local stub server through the installed code.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn, spawnSync, type SpawnSyncOptions } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const pkgDir = fileURLToPath(new URL('..', import.meta.url));

function run(cmd: string, args: string[], opts: SpawnSyncOptions = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', shell: process.platform === 'win32', timeout: 120_000, ...opts });
  return { status: r.status, stdout: String(r.stdout ?? ''), stderr: String(r.stderr ?? '') };
}

const CONSUMER_TS = `
import { SteelmatterClient, SteelmatterApiError, isPermitIssued, type MandateCreateRequest } from '@irontrex/steelmatter-sdk';
const c = new SteelmatterClient({ baseUrl: 'http://127.0.0.1:1', token: 't' });
const req: MandateCreateRequest = { actor_ids: ['a'], audience: 'x', supplier_ids: ['s'], delivery_ids: ['d'], currency: 'INR', limit_minor: 1, expires_at: '2026-10-16T00:00:00Z' };
export async function f(): Promise<string> {
  await c.createMandate(req);
  const r = await c.evaluateAction({ instruction_id: 'i', action_revision: 1 });
  try { return isPermitIssued(r) ? r.data.permit_jws : r.data.reason; } catch (e) { return e instanceof SteelmatterApiError ? e.code : 'x'; }
}
`;

const CONSUMER_MJS = `
import { SteelmatterClient, SteelmatterApiError, formatINR } from '@irontrex/steelmatter-sdk';
const c = new SteelmatterClient({ baseUrl: process.env.STUB_URL, token: 'stub-token', retry: false });
const ok = await c.getAdapterAssurance('synthetic-procurement');
let code = '';
try { await c.getOperation('missing'); } catch (e) { if (e instanceof SteelmatterApiError) code = e.code; }
process.stdout.write(JSON.stringify({ adapter: ok.data.adapter_id, requestId: ok.requestId, code, inr: formatINR(100000000) }));
`;

test('installed package: imports by name, typechecks a consumer and calls the API', async () => {
  const build = run('npm', ['run', 'build'], { cwd: pkgDir });
  assert.equal(build.status, 0, build.stdout + build.stderr);

  const packDir = mkdtempSync(join(tmpdir(), 'sms-pack-'));
  const pack = run('npm', ['pack', '--pack-destination', packDir], { cwd: pkgDir });
  assert.equal(pack.status, 0, pack.stderr);
  const tarball = readdirSync(packDir).find((f) => f.endsWith('.tgz'));
  assert.ok(tarball, 'npm pack produced no tarball');

  const app = mkdtempSync(join(tmpdir(), 'sms-app-'));
  writeFileSync(join(app, 'package.json'), JSON.stringify({ name: 'sms-install-check', private: true, type: 'module' }));
  const install = run('npm', ['install', '--prefer-offline', '--no-audit', '--no-fund', '--ignore-scripts', join(packDir, tarball)], { cwd: app });
  assert.equal(install.status, 0, install.stderr);

  const installed = join(app, 'node_modules', '@irontrex', 'steelmatter-sdk');
  const files = readdirSync(installed).sort();
  assert.deepEqual(files, ['README.md', 'dist', 'package.json'], 'only built output and the README are published');
  const manifest = JSON.parse(readFileSync(join(installed, 'package.json'), 'utf8'));
  assert.equal(manifest.dependencies, undefined, 'the SDK has no runtime dependencies');
  assert.ok(readdirSync(join(installed, 'dist')).includes('index.d.ts'), 'type declarations ship');

  // A TypeScript consumer compiles against the installed declarations.
  writeFileSync(join(app, 'consumer.ts'), CONSUMER_TS);
  const tsc = join(pkgDir, 'node_modules', '.bin', process.platform === 'win32' ? 'tsc.cmd' : 'tsc');
  const typed = run(tsc, ['--noEmit', '--strict', '--module', 'nodenext', '--moduleResolution', 'nodenext', '--target', 'es2023', '--types', '', 'consumer.ts'], { cwd: app });
  assert.equal(typed.status, 0, typed.stdout + typed.stderr);

  // The installed JavaScript talks to a stub server.
  const server = createServer((req, res) => {
    const auth = req.headers.authorization === 'Bearer stub-token';
    res.setHeader('content-type', 'application/json');
    res.setHeader('x-request-id', 'req_' + '1'.repeat(24));
    if (auth && req.method === 'GET' && req.url === '/v1/adapters/synthetic-procurement/assurance') {
      res.end(JSON.stringify({ adapter_id: 'synthetic-procurement', version: 'stub', simulation: true, supported_operations: ['purchase_order.issue'], supported_paths: [], limitations: [], test_references: [] }));
    } else {
      res.statusCode = 404;
      res.end(JSON.stringify({ error_code: 'NOT_FOUND', message: 'operation not found', request_id: 'req_' + '1'.repeat(24), retryable: false }));
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    const port = (server.address() as { port: number }).port;
    writeFileSync(join(app, 'consumer.mjs'), CONSUMER_MJS);
    const child = spawn(process.execPath, ['consumer.mjs'], { cwd: app, env: { ...process.env, STUB_URL: `http://127.0.0.1:${port}` } });
    let out = '';
    let err = '';
    child.stdout.on('data', (d) => (out += d));
    child.stderr.on('data', (d) => (err += d));
    const code = await new Promise<number | null>((resolve) => child.on('close', resolve));
    assert.equal(code, 0, err);
    assert.deepEqual(JSON.parse(out), { adapter: 'synthetic-procurement', requestId: 'req_' + '1'.repeat(24), code: 'NOT_FOUND', inr: 'INR 10,00,000' });
  } finally {
    server.close();
  }
});
