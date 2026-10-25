import test from 'node:test';
import assert from 'node:assert/strict';
import { bindAction, verifyBoundAction, compareObservation } from '../src/index.mjs';
import { createFixture } from '../examples/memory-adapter.mjs';

test('binding verifies and is stable across key ordering', () => {
  const f = createFixture();
  const a = bindAction(f.manifest, f.request);
  assert.deepEqual(verifyBoundAction(f.manifest, a), a);
  assert.equal(bindAction(f.manifest, Object.fromEntries(Object.entries(f.request).reverse())).action_digest, a.action_digest);
  assert.ok(Object.isFrozen(a.parameters));
});

for (const field of ['operation_id', 'tenant_id', 'actor_id', 'destination_id', 'target_id', 'expected_version']) {
  test(`action binding includes ${field}`, () => {
    const f = createFixture();
    const a = bindAction(f.manifest, f.request);
    const changed = { ...a, [field]: 'different' };
    assert.throws(() => verifyBoundAction(f.manifest, changed), /ACTION_BINDING_MISMATCH/);
    assert.notEqual(bindAction(f.manifest, { ...f.request, [field]: 'different' }).action_digest, a.action_digest);
  });
}

test('parameters and manifest revision are bound', () => {
  const f = createFixture();
  const a = bindAction(f.manifest, f.request);
  assert.throws(() => verifyBoundAction(f.manifest, { ...a, parameters: { enabled: true } }), /ACTION_BINDING_MISMATCH/);
  assert.throws(() => verifyBoundAction({ ...f.manifest, adapter_version: '0.2.0' }, a), /ACTION_BINDING_MISMATCH/);
});

test('binding rejects missing, extra, inherited, unsupported and wrong-type fields', () => {
  const f = createFixture();
  const noTarget = { ...f.request }; delete noTarget.target_id;
  for (const req of [
    noTarget, { ...f.request, extra: 'surprise' }, Object.create(f.request),
    { ...f.request, operation: 'not-declared' }, { ...f.request, expected_version: 1 },
    { ...f.request, parameters: { enabled: 'false' } },
    { ...f.request, parameters: { enabled: false, arbitrary: true } },
  ]) assert.throws(() => bindAction(f.manifest, req));
});

test('every supported parameter type is checked without coercion', () => {
  const f = createFixture();
  const m = structuredClone(f.manifest);
  m.operations[0].parameters = { name: 'string', count: 'integer', enabled: 'boolean' };
  const parameters = { name: 'example', count: 2, enabled: false };
  assert.equal(bindAction(m, { ...f.request, parameters }).parameters.count, 2);
  for (const replacement of [{ name: '' }, { name: 1 }, { count: '2' }, { count: 1.5 }, { enabled: 0 }]) {
    assert.throws(() => bindAction(m, { ...f.request, parameters: { ...parameters, ...replacement } }));
  }
});

test('execution acknowledgement alone never qualifies as verified readback', async () => {
  const f = createFixture(); const a = f.action(); const e = await f.adapter.execute(a);
  assert.equal(compareObservation(f.manifest, a, e, e), 'INSUFFICIENT_EVIDENCE');
  assert.equal(compareObservation(f.manifest, a, e, null), 'INSUFFICIENT_EVIDENCE');
  assert.equal(compareObservation(f.manifest, a, { status: 'UNKNOWN' }, await f.adapter.observe(a)), 'INSUFFICIENT_EVIDENCE');
});

for (const field of ['operation_id', 'action_digest', 'tenant_id', 'destination_id', 'target_id', 'target_version']) {
  test(`readback mismatch in ${field} is not accepted`, async () => {
    const f = createFixture(); const a = f.action(); const e = await f.adapter.execute(a);
    const o = { ...await f.adapter.observe(a), [field]: 'wrong' };
    assert.equal(compareObservation(f.manifest, a, e, o), 'MISMATCH');
  });
}

test('all material readback values are required and compared', async () => {
  const f = createFixture(); const a = f.action(); const e = await f.adapter.execute(a);
  const o = await f.adapter.observe(a);
  assert.equal(compareObservation(f.manifest, a, e, o), 'MATCH');
  assert.equal(compareObservation(f.manifest, a, e, { ...o, values: {} }), 'INSUFFICIENT_EVIDENCE');
  assert.equal(compareObservation(f.manifest, a, e, { ...o, values: { enabled: true } }), 'MISMATCH');
  assert.equal(compareObservation(f.manifest, a, { ...e, action_digest: 'wrong' }, o), 'MISMATCH');
  assert.equal(compareObservation(f.manifest, a,
    { ...e, target_version: a.expected_version }, { ...o, target_version: a.expected_version }), 'MISMATCH');
});
