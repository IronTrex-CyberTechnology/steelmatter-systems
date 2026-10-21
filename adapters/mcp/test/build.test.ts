// Strict typecheck (sources and tests) and the compiled entry point, so type
// errors and a broken build fail the mcp phase like any other test.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = fileURLToPath(new URL('..', import.meta.url));
const tsc = join(dir, 'node_modules', '.bin', process.platform === 'win32' ? 'tsc.cmd' : 'tsc');
const run = (cmd: string, args: string[], env?: NodeJS.ProcessEnv) =>
  spawnSync(cmd, args, { cwd: dir, encoding: 'utf8', shell: process.platform === 'win32', timeout: 120_000, input: '', ...(env ? { env } : {}) });

test('strict typecheck of sources and tests', () => {
  const r = run(tsc, ['-p', 'tsconfig.json']);
  assert.equal(r.status, 0, String(r.stdout) + String(r.stderr));
});

test('the build compiles and the built steelmatter-mcp entry point runs', () => {
  const b = run(tsc, ['-p', 'tsconfig.build.json']);
  assert.equal(b.status, 0, String(b.stdout) + String(b.stderr));
  const r = run(process.execPath, ['dist/main.js'], { PATH: process.env.PATH ?? '', STEELMATTER_MCP_TOKEN: 'synthetic-token' });
  assert.equal(r.status, 0, String(r.stderr)); // empty stdin: starts, then exits cleanly
  assert.match(String(r.stderr), /ready; service http:\/\/127\.0\.0\.1:8080/);
});
