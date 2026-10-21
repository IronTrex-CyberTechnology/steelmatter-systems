// The demo builds from the pinned fixtures and typechecks strictly, so a
// broken page or type error fails the demo phase like any other test.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { buildSite, data, tempDir } from './helpers.ts';

const dir = fileURLToPath(new URL('..', import.meta.url));
const tsc = join(dir, 'node_modules', '.bin', process.platform === 'win32' ? 'tsc.cmd' : 'tsc');

test('strict typecheck of sources and tests', () => {
  const r = spawnSync(tsc, ['-p', 'tsconfig.json'], { cwd: dir, encoding: 'utf8', shell: process.platform === 'win32', timeout: 120_000 });
  assert.equal(r.status, 0, String(r.stdout) + String(r.stderr));
});

test('the site builds into a static page that needs no server or network', async () => {
  const out = await buildSite(tempDir('site'));
  const html = readFileSync(join(out, 'index.html'), 'utf8');
  assert.match(html, /<script src="app\.js"><\/script>/);
  assert.match(html, /<link rel="stylesheet" href="style\.css">/);
  assert.doesNotMatch(html, /https?:\/\//, 'the page must not load anything from the network');
  const app = readFileSync(join(out, 'app.js'), 'utf8');
  for (const banned of ['fetch(', 'XMLHttpRequest', 'WebSocket', 'sendBeacon', 'createRemoteJWKSet']) {
    assert.equal(app.includes(banned), false, `app.js uses ${banned}`);
  }
});

test('the page embeds the pinned recording and evidence fixtures unchanged', async () => {
  const out = await buildSite(tempDir('embed'));
  const app = readFileSync(join(out, 'app.js'), 'utf8');
  assert.ok(app.includes(data.reference.sha256), 'recording digest is embedded');
  assert.ok(app.includes(data.evidence.sha256), 'evidence fixture digest is embedded');
  assert.ok(app.includes(JSON.stringify(data.reference.data.generated_at_commit)), 'recorded commit is embedded');
  assert.equal(data.reference.data.simulation, true);
  assert.equal(data.reference.data.synthetic, true);
});
