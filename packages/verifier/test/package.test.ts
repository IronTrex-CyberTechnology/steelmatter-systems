// Installed-package checks (I07 standalone package and CLI; T42 verifier
// distribution): build, pack, install the tarball into an empty Node project
// with no engine, database or model API, then use only the installed package.
// Tests elsewhere run from src/; these prove the published form works.
// The install is shared by every test in this file (node --test runs files in
// parallel, so the build must not be split across files).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, readdirSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync, type SpawnSyncOptions } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const pkgDir = fileURLToPath(new URL('..', import.meta.url));
const vectorsDir = fileURLToPath(new URL('../../../fixtures/vectors/', import.meta.url));
const fx = JSON.parse(readFileSync(join(vectorsDir, 'evidence-v1.json'), 'utf8'));
const byName = (n: string) => fx.cases.find((c: { name: string }) => c.name === n).bundle;

function run(cmd: string, args: string[], opts: SpawnSyncOptions = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', shell: process.platform === 'win32', timeout: 120_000, ...opts });
  return { status: r.status, stdout: String(r.stdout ?? ''), stderr: String(r.stderr ?? '') };
}

// Preloaded into every process that runs the installed package: any attempt
// to open a socket, resolve a name or fetch is recorded and refused.
const NETWORK_GUARD = `
import { appendFileSync } from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';
import net from 'node:net'; import tls from 'node:tls'; import dns from 'node:dns';
import http from 'node:http'; import https from 'node:https'; import dgram from 'node:dgram';
const log = process.env.SMV_NET_LOG;
function block(name) {
  return function () {
    if (log) appendFileSync(log, name + '\\n');
    throw new Error('network disabled by test guard: ' + name);
  };
}
net.Socket.prototype.connect = block('net.Socket.connect');
net.connect = net.createConnection = block('net.connect');
tls.connect = block('tls.connect');
dns.lookup = dns.resolve = block('dns');
dns.promises.lookup = dns.promises.resolve = block('dns.promises');
http.request = http.get = block('http.request');
https.request = https.get = block('https.request');
dgram.createSocket = block('dgram.createSocket');
globalThis.fetch = async () => block('fetch')();
syncBuiltinESMExports();
`;

// Runs every golden vector through the installed package, imported by name.
const GOLDEN_CHECK = `
import { readFileSync } from 'node:fs';
import { validate, canonicalizeRaw, digest, digestContext, verify, protectedHeader, signingInput, loadTrustStore,
  verifyBundle, ContractError } from '@irontrex/steelmatter-verifier';
const golden = JSON.parse(readFileSync('golden-v1.json', 'utf8'));
const evidence = JSON.parse(readFileSync('evidence-v1.json', 'utf8'));
const bytes = (b64) => new Uint8Array(Buffer.from(b64, 'base64url'));
const code = async (fn) => { try { await fn(); return 'ACCEPTED'; } catch (e) { if (e instanceof ContractError) return e.code; throw e; } };
const bad = []; const counts = { canonical: 0, payload_negative: 0, jws_positive: 0, jws_negative: 0, evidence: 0 };
const trust = await loadTrustStore(golden.trust_store);
for (const v of golden.canonical) {
  counts.canonical++;
  const input = bytes(v.input_b64); validate(v.kind, input);
  const c = canonicalizeRaw(input);
  if (Buffer.from(c).toString('utf8') !== v.canonical || digestContext(v.kind) !== v.digest_context || digest(v.kind, c) !== v.digest) bad.push('canonical ' + v.name);
}
for (const v of golden.payload_negative) {
  counts.payload_negative++;
  const got = await code(() => validate(v.kind, bytes(v.input_b64)));
  if (got !== v.expected_code) bad.push('payload negative ' + v.name + ': ' + got);
}
for (const v of golden.jws_positive) {
  counts.jws_positive++;
  const payload = bytes(v.jws.split('.')[1]);
  const r = await verify(v.jws, v.kind, trust);
  if (protectedHeader(v.kind, v.kid) !== v.protected_header || signingInput(v.kind, v.kid, payload) !== v.signing_input ||
      r.digest !== v.payload_digest || r.kid !== v.kid) bad.push('jws positive ' + v.name);
}
for (const v of golden.jws_negative) {
  counts.jws_negative++;
  const got = await code(() => verify(v.jws, v.kind, trust));
  if (got !== v.expected_code) bad.push('jws negative ' + v.name + ': ' + got);
}
const etrust = await loadTrustStore(evidence.trust_store);
for (const c of evidence.cases) {
  counts.evidence++;
  const r = await verifyBundle(c.bundle, etrust);
  if (r.linkage.status !== c.expect.linkage) bad.push('evidence ' + c.name + ' linkage ' + r.linkage.status);
  if (c.expect.observation && r.observation.status !== c.expect.observation) bad.push('evidence ' + c.name + ' observation ' + r.observation.status);
}
// Explicit trust anchors: an empty trust store trusts nothing, and no store at all is refused.
const empty = await loadTrustStore({ trust_store_version: '1', keys: [] });
const none = await verifyBundle(evidence.cases[0].bundle, empty);
if (Object.values(none.records).some((x) => x.issuer === 'TRUSTED' || x.integrity === 'VALID')) bad.push('empty trust store trusted a record');
let refused = false;
try { const r = await verifyBundle(evidence.cases[0].bundle, undefined); refused = !Object.values(r.records).some((x) => x.issuer === 'TRUSTED'); }
catch { refused = true; }
if (!refused) bad.push('verification without a trust store trusted a record');
process.stdout.write(JSON.stringify({ bad, counts }));
`;

let installed: { app: string; bin: string; env: NodeJS.ProcessEnv; netLog: string } | undefined;

function install() {
  if (installed) return installed;
  const build = run('npm', ['run', 'build'], { cwd: pkgDir });
  assert.equal(build.status, 0, build.stdout + build.stderr);

  const packDir = mkdtempSync(join(tmpdir(), 'smv-pack-'));
  const pack = run('npm', ['pack', '--pack-destination', packDir], { cwd: pkgDir });
  assert.equal(pack.status, 0, pack.stderr);
  const tarball = readdirSync(packDir).find((f) => f.endsWith('.tgz'));
  assert.ok(tarball, 'npm pack produced no tarball');

  const app = mkdtempSync(join(tmpdir(), 'smv-app-'));
  writeFileSync(join(app, 'package.json'), JSON.stringify({ name: 'smv-install-check', private: true, type: 'module' }));
  const inst = run('npm', ['install', '--prefer-offline', '--no-audit', '--no-fund', '--ignore-scripts', join(packDir, tarball)], { cwd: app });
  assert.equal(inst.status, 0, inst.stderr);

  writeFileSync(join(app, 'no-network.mjs'), NETWORK_GUARD);
  const netLog = join(app, 'network-attempts.log');
  const env = { ...process.env, NODE_OPTIONS: `--import=${join(app, 'no-network.mjs')}`, SMV_NET_LOG: netLog };
  const bin = join(app, 'node_modules', '.bin', process.platform === 'win32' ? 'steelmatter-verify.cmd' : 'steelmatter-verify');
  installed = { app, bin, env, netLog };
  return installed;
}

function networkAttempts(netLog: string): string[] {
  return existsSync(netLog) ? readFileSync(netLog, 'utf8').split('\n').filter(Boolean) : [];
}

test('installed package: built tarball installs, imports by name and runs steelmatter-verify', () => {
  const { app, bin } = install();
  const pkg = join(app, 'node_modules', '@irontrex', 'steelmatter-verifier');
  assert.deepEqual(readdirSync(pkg).filter((f) => f === 'src'), [], 'TypeScript source must not be published');

  const imp = run(process.execPath, ['--input-type=module', '-e',
    "const m = await import('@irontrex/steelmatter-verifier'); if (typeof m.verifyBundle !== 'function') process.exit(3);"], { cwd: app });
  assert.equal(imp.status, 0, imp.stderr);

  writeFileSync(join(app, 't.json'), JSON.stringify(fx.trust_store));
  writeFileSync(join(app, 'complete.json'), JSON.stringify(byName('complete')));
  writeFileSync(join(app, 'broken.json'), JSON.stringify(byName('broken-permit-link')));

  const ok = run(bin, ['verify', '--trust', 't.json', '--bundle', 'complete.json', '--now', fx.usable_at,
    '--fail-on', 'integrity,issuer,profile,linkage,observation'], { cwd: app });
  assert.equal(ok.status, 0, ok.stdout + ok.stderr);
  assert.match(ok.stdout, /linkage: COMPLETE/);

  const broken = run(bin, ['verify', '--trust', 't.json', '--bundle', 'broken.json', '--fail-on', 'linkage'], { cwd: app });
  assert.equal(broken.status, 1, broken.stdout + broken.stderr);
});

test('T42: the network guard records and refuses network use (control)', () => {
  const { app, env } = install();
  const log = join(app, 'control.log');
  const r = run(process.execPath, ['--input-type=module', '-e',
    "try { await fetch('http://127.0.0.1:9/'); } catch {} try { (await import('node:net')).connect(9, '127.0.0.1'); } catch {}"],
  { cwd: app, env: { ...env, SMV_NET_LOG: log } });
  assert.equal(r.status, 0, r.stderr);
  assert.deepEqual(networkAttempts(log), ['fetch', 'net.connect']);
});

test('T42: installed package reproduces every golden valid/invalid vector with no network calls', () => {
  const { app, env, netLog } = install();
  for (const f of ['golden-v1.json', 'evidence-v1.json']) writeFileSync(join(app, f), readFileSync(join(vectorsDir, f)));
  writeFileSync(join(app, 'golden-check.mjs'), GOLDEN_CHECK);
  const r = run(process.execPath, ['golden-check.mjs'], { cwd: app, env });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  const { bad, counts } = JSON.parse(r.stdout);
  assert.deepEqual(bad, []);
  for (const [k, n] of Object.entries(counts)) assert.ok((n as number) > 0, `no ${k} vectors ran`);
  assert.deepEqual(networkAttempts(netLog), [], 'the verifier attempted network access');
});

test('T42: the installed CLI requires explicit trust anchors and makes no network calls', () => {
  const { app, bin, env, netLog } = install();
  writeFileSync(join(app, 't.json'), JSON.stringify(fx.trust_store));
  writeFileSync(join(app, 'complete.json'), JSON.stringify(byName('complete')));

  const noTrust = run(bin, ['verify', '--bundle', 'complete.json'], { cwd: app, env });
  assert.equal(noTrust.status, 2, noTrust.stdout + noTrust.stderr);
  assert.match(noTrust.stderr, /trust anchors are never fetched/);

  const ok = run(bin, ['verify', '--trust', 't.json', '--bundle', 'complete.json', '--now', fx.usable_at, '--format', 'json',
    '--fail-on', 'integrity,issuer,profile,linkage,observation'], { cwd: app, env });
  assert.equal(ok.status, 0, ok.stdout + ok.stderr);
  const report = JSON.parse(ok.stdout);
  assert.ok(Object.values(report.records).every((x: any) => x.issuer === 'TRUSTED'));
  assert.deepEqual(networkAttempts(netLog), [], 'the CLI attempted network access');
});
