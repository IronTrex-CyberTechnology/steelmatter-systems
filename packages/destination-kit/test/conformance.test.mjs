import test from 'node:test';
import assert from 'node:assert/strict';
import { bindAction } from '../src/index.mjs';
import { runConformance } from '../src/conformance.mjs';
import { createFixture } from '../examples/memory-adapter.mjs';

test('synthetic reference passes all eight fixture conformance scenarios', async t => {
  const report = await runConformance(createFixture);
  assert.equal(report.certification, false);
  assert.equal(report.results.length, 8);
  for (const result of report.results) {
    await t.test(result.name, () => assert.equal(result.status, 'PASS', result.message));
  }
  assert.equal(report.passed, true);
});

test('broken replay behavior fails conformance; results are not silently skipped', async () => {
  const report = await runConformance(() => {
    const f = createFixture();
    return { ...f, writeCount: () => 2 };
  });
  assert.equal(report.passed, false);
  assert.ok(report.results.some(r => r.name === 'identical-replay' && r.status === 'FAIL'));
});

test('conformance refuses external manifests without calling the adapter', async () => {
  let calls = 0;
  const report = await runConformance(() => {
    const f = createFixture();
    return { ...f, manifest: { ...f.manifest, environment: 'external' }, adapter: { execute: () => { calls++; } } };
  });
  assert.equal(report.passed, false);
  assert.equal(calls, 0);
});

for (const field of ['tenant_id', 'destination_id', 'target_id']) {
  test(`synthetic destination rejects ${field} scope substitution without a write`, async () => {
    const f = createFixture(); const a = bindAction(f.manifest, { ...f.request, [field]: 'different' });
    for (const method of Object.values(f.adapter)) await assert.rejects(method(a), /DESTINATION_SCOPE_MISMATCH/);
    assert.equal(f.writeCount(), 0);
  });
}

test('two different operations racing for one version cannot both change it', async () => {
  const f = createFixture();
  const [a, b] = await Promise.all([
    f.adapter.execute(f.action()),
    f.adapter.execute(bindAction(f.manifest, { ...f.request, operation_id: 'operation-2' })),
  ]);
  assert.deepEqual([a.status, b.status].sort(), ['APPLIED', 'NOT_APPLIED']);
  assert.equal(f.writeCount(), 1);
});

test('reconcile and observe do not perform a write', async () => {
  const f = createFixture(); const a = f.action();
  for (let i = 0; i < 3; i++) {
    assert.equal((await f.adapter.reconcile(a)).status, 'UNKNOWN');
    assert.equal(await f.adapter.observe(a), null);
  }
  assert.equal(f.writeCount(), 0);
});
