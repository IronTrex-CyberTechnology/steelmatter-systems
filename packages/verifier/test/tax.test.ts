// D1 v1.4 action fields (R1 decisions 1 and 2), shared with Go via
// fixtures/vectors/action-tax-v1.json (Go half: core/contract/tax_test.go).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { validate, ContractError } from '../src/index.ts';

const enc = new TextEncoder();
const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/action-tax-v1.json', import.meta.url), 'utf8'));

test('tax fixture covers accepted and refused actions', () => {
  assert.ok(fx.cases.some((c: any) => c.expect === 'accept'));
  assert.ok(fx.cases.some((c: any) => c.expect !== 'accept'));
});

for (const c of fx.cases) {
  test(`action tax ${c.name}`, () => {
    const input = enc.encode(JSON.stringify(c.payload));
    if (c.expect === 'accept') {
      assert.doesNotThrow(() => validate('action', input));
    } else {
      assert.throws(() => validate('action', input), (e: unknown) => e instanceof ContractError && e.code === c.expect);
    }
  });
}
