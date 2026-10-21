// CLR-01 nesting-depth boundary, shared with Go via
// fixtures/vectors/depth-v1.json (Go half: core/contract/depth_test.go).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { parseStrict, ContractError } from '../src/index.ts';
import { MAX_DEPTH } from '../src/strict-json.ts';

const enc = new TextEncoder();
const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/depth-v1.json', import.meta.url), 'utf8'));

test('depth fixture matches MAX_DEPTH and covers both sides of the boundary', () => {
  assert.equal(fx.max_depth, MAX_DEPTH);
  assert.ok(fx.cases.some((c: any) => c.expect === 'accept'));
  assert.ok(fx.cases.some((c: any) => c.expect !== 'accept'));
});

for (const c of fx.cases) {
  test(`depth ${c.name}`, () => {
    const input = c.prefix + '['.repeat(c.arrays) + c.inner + ']'.repeat(c.arrays) + c.suffix;
    if (c.expect === 'accept') {
      assert.doesNotThrow(() => parseStrict(enc.encode(input)), `deepest value at depth ${c.deepest} must be accepted`);
    } else {
      assert.throws(() => parseStrict(enc.encode(input)), (e: unknown) => e instanceof ContractError && e.code === c.expect);
    }
  });
}
