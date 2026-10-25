// Destructive *fixture* tests. Only use disposable synthetic destinations.
// A passing result covers these scenarios, not production certification.
import assert from 'node:assert/strict';
import { bindAction, compareObservation, validateManifest } from './index.mjs';

export async function runConformance(createFixture) {
  const checks = [
    ['apply-and-readback', async f => {
      const a = bindAction(f.manifest, f.request);
      assert.equal((await f.adapter.precheck(a)).status, 'READY');
      const e = await f.adapter.execute(a);
      assert.equal(e.status, 'APPLIED');
      assert.equal(compareObservation(f.manifest, a, e, await f.adapter.observe(a)), 'MATCH');
      assert.equal(f.writeCount(), 1);
    }],
    ['identical-replay', async f => {
      const a = bindAction(f.manifest, f.request);
      const first = await f.adapter.execute(a);
      assert.deepEqual(await f.adapter.execute(a), first);
      assert.equal(f.writeCount(), 1);
    }],
    ['operation-id-conflict', async f => {
      const a = bindAction(f.manifest, f.request);
      await f.adapter.execute(a);
      const changed = bindAction(f.manifest, { ...f.request, expected_version: 'changed-version' });
      await assert.rejects(f.adapter.execute(changed), /OPERATION_CONFLICT/);
      assert.equal(f.writeCount(), 1);
    }],
    ['change-after-precheck', async f => {
      const a = bindAction(f.manifest, f.request);
      assert.equal((await f.adapter.precheck(a)).status, 'READY');
      f.mutate();
      const e = await f.adapter.execute(a);
      assert.equal(e.status, 'NOT_APPLIED');
      assert.equal(e.reason, 'STALE_VERSION');
      assert.equal(f.writeCount(), 0);
    }],
    ['lost-response-reconcile-without-write', async f => {
      const a = bindAction(f.manifest, f.request);
      f.loseNextResponse();
      await assert.rejects(f.adapter.execute(a), /SIMULATED_LOST_RESPONSE/);
      const e = await f.adapter.reconcile(a);
      assert.equal(e.status, 'APPLIED');
      assert.equal(compareObservation(f.manifest, a, e, await f.adapter.observe(a)), 'MATCH');
      assert.equal(f.writeCount(), 1);
    }],
    ['unknown-stays-unknown', async f => {
      const a = bindAction(f.manifest, f.request);
      assert.equal((await f.adapter.reconcile(a)).status, 'UNKNOWN');
      f.loseNextResponse();
      await assert.rejects(f.adapter.execute(a), /SIMULATED_LOST_RESPONSE/);
      f.setReadUnavailable(true);
      assert.equal((await f.adapter.reconcile(a)).status, 'UNKNOWN');
      assert.equal(await f.adapter.observe(a), null);
      assert.equal(f.writeCount(), 1);
      f.setReadUnavailable(false);
      assert.equal((await f.adapter.reconcile(a)).status, 'APPLIED');
      assert.equal(f.writeCount(), 1);
    }],
    ['readback-drift', async f => {
      const a = bindAction(f.manifest, f.request);
      const e = await f.adapter.execute(a);
      f.mutate();
      assert.equal(compareObservation(f.manifest, a, e, await f.adapter.observe(a)), 'MISMATCH');
    }],
    ['concurrent-identical-attempts', async f => {
      const a = bindAction(f.manifest, f.request);
      const [first, second] = await Promise.all([f.adapter.execute(a), f.adapter.execute(a)]);
      assert.deepEqual(first, second);
      assert.equal(f.writeCount(), 1);
    }],
  ];
  const results = [];
  for (const [name, check] of checks) {
    try {
      const f = await createFixture();
      assert.equal(validateManifest(f.manifest).environment, 'synthetic', 'conformance requires synthetic fixtures');
      await check(f);
      results.push({ name, status: 'PASS' });
    } catch (error) {
      results.push({ name, status: 'FAIL', message: error instanceof Error ? error.message : 'fixture threw a non-Error value' });
    }
  }
  return { profile: 'steelmatter.destination-fixture-tests/0.1', certification: false, results, passed: results.every(r => r.status === 'PASS') };
}
