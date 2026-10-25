import test from 'node:test';
import assert from 'node:assert/strict';
import { canonical, parseJSON, snapshot, MAX_PAYLOAD_BYTES } from '../src/json.mjs';

for (const [name, text] of [
  ['duplicate key', '{"a":1,"a":2}'],
  ['escaped duplicate key', '{"a":1,"\\u0061":2}'],
  ['fraction', '1.5'], ['exponent', '1e3'], ['negative zero', '-0'],
  ['unsafe integer', '9007199254740992'], ['trailing value', '{}{}'],
  ['trailing comma', '{"a":1,}'], ['byte order mark', '\ufeff{}'],
  ['lone surrogate escape', '"\\ud800"'], ['invalid escape', '"\\q"'],
  ['deep nesting', '['.repeat(34) + '0' + ']'.repeat(34)],
]) test(`strict parser rejects ${name}`, () => assert.throws(() => parseJSON(Buffer.from(text))));

test('strict parser rejects malformed UTF-8, oversized input and non-bytes', () => {
  assert.throws(() => parseJSON(Buffer.from([0xff])));
  assert.throws(() => parseJSON(Buffer.alloc(MAX_PAYLOAD_BYTES + 1)));
  assert.throws(() => parseJSON('{}'));
});

test('strict parser retains safe integers, paired unicode and whitespace', () => {
  const data = parseJSON(Buffer.from(' {"n":9007199254740991,"s":"\\ud83d\\ude80","a":[true,null,-1]} \n'));
  assert.equal(data.s, '🚀');
  assert.equal(data.n, Number.MAX_SAFE_INTEGER);
  assert.deepEqual(data.a, [true, null, -1]);
});

test('snapshot is a deep-frozen detached value', () => {
  const source = { nested: { value: 1 } };
  const value = snapshot(source);
  source.nested.value = 2;
  assert.equal(value.nested.value, 1);
  assert.throws(() => { value.nested.value = 3; });
});

test('canonical encoding sorts keys and does not use toJSON or getters', () => {
  assert.equal(canonical({ z: 1, a: true }), '{"a":true,"z":1}');
  let calls = 0;
  assert.throws(() => canonical({ get a() { calls++; return 1; } }));
  assert.throws(() => canonical({ toJSON() { calls++; return {}; } }));
  assert.equal(calls, 0);
});

for (const [name, value] of [
  ['undefined', undefined], ['NaN', NaN], ['Infinity', Infinity], ['negative zero', -0],
  ['unsafe integer', 9007199254740992], ['fraction', 0.5], ['bigint', 1n],
  ['function', () => {}], ['date', new Date(0)], ['lone surrogate', '\ud800'],
  ['sparse array', new Array(2)], ['symbol property', { [Symbol('a')]: 1 }],
]) test(`canonical encoding rejects ${name}`, () => assert.throws(() => canonical(value)));

test('reject cycles, non-enumerable fields, extended arrays and excessive data', () => {
  const cycle = {}; cycle.self = cycle;
  const hidden = Object.defineProperty({}, 'secret', { value: 1 });
  const array = [1]; array.extra = 2;
  for (const value of [cycle, hidden, array, 'x'.repeat(MAX_PAYLOAD_BYTES + 1)]) assert.throws(() => snapshot(value));
});
