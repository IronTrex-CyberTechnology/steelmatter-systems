import test from 'node:test';
import assert from 'node:assert/strict';
import { validateManifest, parseManifest, inspectManifest, manifestDigest, scaffold } from '../src/index.mjs';
import { manifest } from '../examples/memory-adapter.mjs';
const copy = () => structuredClone(manifest);

test('manifest validates declarations, not authorization or implementation', () => {
  const report = inspectManifest(manifest);
  assert.equal(report.declaration, 'CAPABILITIES_DECLARED');
  assert.equal(report.authorization_granted, false);
  assert.equal(report.implementation_verified, false);
  assert.equal(report.blockers.length, 0);
  assert.ok(Object.isFrozen(validateManifest(copy()).operations[0].parameters));
});

test('scaffold is deterministic, blocked and fails closed at every method', async () => {
  assert.deepEqual(scaffold('test-adapter'), scaffold('test-adapter'));
  const files = scaffold('test-adapter');
  const m = parseManifest(Buffer.from(files['manifest.json']));
  assert.equal(inspectManifest(m).declaration, 'BLOCKED');
  assert.equal(inspectManifest(m).blockers.length, 4);
  const module = await import(`data:text/javascript,${encodeURIComponent(files['adapter.mjs'])}`);
  for (const method of Object.values(module.createAdapter())) await assert.rejects(method(), /ADAPTER_NOT_IMPLEMENTED/);
});

for (const [name, mutate] of [
  ['unknown field', m => { m.extra = true; }],
  ['missing field', m => { delete m.limitations; }],
  ['unknown schema', m => { m.schema = 'future'; }],
  ['invalid identifier', m => { m.adapter_id = '../unsafe'; }],
  ['invalid version', m => { m.adapter_version = 'next'; }],
  ['unknown environment', m => { m.environment = 'production-ready'; }],
  ['empty operations', m => { m.operations = []; }],
  ['duplicate operations', m => { m.operations.push(m.operations[0]); }],
  ['unknown operation field', m => { m.operations[0].script = 'unsafe'; }],
  ['payment effect', m => { m.operations[0].effect = 'payment'; }],
  ['unknown capability', m => { m.operations[0].precondition = 'best-effort'; }],
  ['unsupported parameter type', m => { m.operations[0].parameters.enabled = 'function'; }],
  ['unsafe parameter name', m => { m.operations[0].parameters.constructor = 'string'; }],
  ['empty parameters', m => { m.operations[0].parameters = {}; }],
  ['absent limitations', m => { m.limitations = []; }],
  ['empty limitation', m => { m.limitations = ['']; }],
]) {
  test(`manifest rejects ${name}`, () => { const m = copy(); mutate(m); assert.throws(() => validateManifest(m)); });
}

test('object order does not change manifest digest; material declarations do', () => {
  const reordered = Object.fromEntries(Object.entries(manifest).reverse());
  assert.equal(manifestDigest(reordered), manifestDigest(manifest));
  const changed = copy(); changed.adapter_version = '0.1.1';
  assert.notEqual(manifestDigest(changed), manifestDigest(manifest));
});

test('external declarations are not mislabelled as implementation verification', () => {
  const m = copy(); m.environment = 'external';
  assert.equal(inspectManifest(m).implementation_verified, false);
});

for (const id of ['../escape', 'bad/id', 'has space', '', 'x'.repeat(97), "x';console.log('bad')"]) {
  test(`scaffold rejects unsafe identifier ${JSON.stringify(id)}`, () => assert.throws(() => scaffold(id)));
}
