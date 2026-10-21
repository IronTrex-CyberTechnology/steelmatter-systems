// Strict tsc over src, tests and examples (tsconfig.json), so a type error
// fails the sdk phase like any other test. This includes test/types.test.ts,
// whose expected-error directives pin the public types.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const pkgDir = fileURLToPath(new URL('..', import.meta.url));

test('strict typecheck of sources, tests and examples', () => {
  const tsc = join(pkgDir, 'node_modules', '.bin', process.platform === 'win32' ? 'tsc.cmd' : 'tsc');
  const r = spawnSync(tsc, ['-p', 'tsconfig.json'], { cwd: pkgDir, encoding: 'utf8', shell: process.platform === 'win32', timeout: 120_000 });
  assert.equal(r.status, 0, String(r.stdout) + String(r.stderr));
});
