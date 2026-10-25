import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { scaffold } from '../src/index.mjs';
import { manifest } from '../examples/memory-adapter.mjs';

const cli = fileURLToPath(new URL('../src/cli.mjs', import.meta.url));
const run = (...args) => spawnSync(process.execPath, [cli, ...args], { encoding: 'utf8', timeout: 5000 });

test('CLI scaffolds without running or writing an adapter', () => {
  const result = run('scaffold', 'sample-adapter');
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), JSON.parse(JSON.stringify(scaffold('sample-adapter'))));
});

test('CLI distinguishes declared capabilities, blockers and invalid input', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'steelmatter-kit-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, 'manifest.json');
  await writeFile(path, JSON.stringify(manifest));
  let result = run('inspect', path);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).implementation_verified, false);
  await writeFile(path, scaffold('sample-adapter')['manifest.json']);
  result = run('inspect', path);
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).declaration, 'BLOCKED');
  await writeFile(path, '{"adapter_id":"a","adapter_id":"b"}');
  result = run('inspect', path);
  assert.equal(result.status, 2);
  assert.match(result.stderr, /DUPLICATE_KEY/);
  assert.equal(result.stdout, '');
});

test('CLI rejects non-files and oversized files without unbounded reads', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'steelmatter-kit-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  assert.equal(run('inspect', dir).status, 2);
  const path = join(dir, 'large.json');
  await writeFile(path, ' '.repeat((64 << 10) + 1));
  const result = run('inspect', path);
  assert.equal(result.status, 2);
  assert.match(result.stderr, /64 KiB/);
});

test('CLI rejects unsupported commands, missing files and extra arguments', () => {
  for (const args of [[], ['execute', 'anything'], ['scaffold', 'sample', 'extra'], ['inspect', 'this-file-does-not-exist']]) {
    const result = run(...args);
    assert.equal(result.status, 2);
    assert.equal(result.stdout, '');
  }
});

test('demo reports a recovered synthetic effect with one write', () => {
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('../examples/demo.mjs', import.meta.url))], { encoding: 'utf8', timeout: 5000 });
  assert.equal(result.status, 0, result.stderr);
  const report = JSON.parse(result.stdout.slice(result.stdout.indexOf('{')));
  assert.equal(report.synthetic, true);
  assert.equal(report.authorization_performed, false);
  assert.equal(report.signed_evidence, false);
  assert.equal(report.execution, 'APPLIED');
  assert.equal(report.observation, 'MATCH');
  assert.equal(report.destination_writes, 1);
});
