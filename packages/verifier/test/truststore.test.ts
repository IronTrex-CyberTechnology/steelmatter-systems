// QA-D1-002: TypeScript trust-store parsing agrees with Go on every case in
// fixtures/vectors/truststore-v1.json (Go half: tests/conformance).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { loadTrustStore } from '../src/index.ts';

const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/truststore-v1.json', import.meta.url), 'utf8'));
const enc = new TextEncoder();

async function outcome(store: string): Promise<string> {
  try {
    await loadTrustStore(enc.encode(store));
    return 'accept';
  } catch {
    return 'reject';
  }
}

for (const c of fx.cases) {
  test(`trust store ${c.name}`, async () => {
    assert.equal(await outcome(c.store), c.expect);
  });
}

// The QA reproduction cases by name (D03 duplicate kid, D04 duplicate name).
test('QA-D1-002 D03: duplicate kid is rejected', async () => {
  const c = fx.cases.find((x: any) => x.name === 'duplicate-kid');
  await assert.rejects(loadTrustStore(enc.encode(c.store)), /repeats kid/);
});

test('QA-D1-002 D04: duplicate name is rejected', async () => {
  const c = fx.cases.find((x: any) => x.name === 'duplicate-name');
  await assert.rejects(loadTrustStore(enc.encode(c.store)), /repeats kid .* or name/);
});

test('a parsed object (not bytes) is held to the same rules', async () => {
  const c = fx.cases.find((x: any) => x.name === 'duplicate-kid-different-purposes');
  await assert.rejects(loadTrustStore(JSON.parse(c.store)));
});
