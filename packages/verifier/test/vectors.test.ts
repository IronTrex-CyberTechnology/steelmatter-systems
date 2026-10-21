// Runs every golden vector (fixtures/vectors/golden-v1.json) through the
// TypeScript implementation. The Go engine runs the same file in
// tests/conformance; agreement on every expectation is the T21/T41 contract.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  validate, canonicalizeRaw, digest, digestContext, verify, protectedHeader, signingInput,
  loadTrustStore, ContractError, type Kind, type SignedKind,
} from '../src/index.ts';

const vectors = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/golden-v1.json', import.meta.url), 'utf8'));
const bytes = (b64: string) => new Uint8Array(Buffer.from(b64, 'base64url'));
const trust = await loadTrustStore(vectors.trust_store);

function code(fn: () => unknown): string {
  try {
    fn();
  } catch (e) {
    if (e instanceof ContractError) return e.code;
    throw e;
  }
  return 'ACCEPTED';
}
async function codeAsync(fn: () => Promise<unknown>): Promise<string> {
  try {
    await fn();
  } catch (e) {
    if (e instanceof ContractError) return e.code;
    throw e;
  }
  return 'ACCEPTED';
}

test('vector file shape', () => {
  assert.equal(vectors.vector_set, 'steelmatter-golden-v1');
  assert.ok(vectors.canonical.length > 0 && vectors.payload_negative.length > 0);
  assert.ok(vectors.jws_positive.length > 0 && vectors.jws_negative.length > 0);
});

for (const v of vectors.canonical) {
  test(`canonical ${v.name}`, () => {
    const input = bytes(v.input_b64);
    validate(v.kind as Kind, input);
    const canonical = canonicalizeRaw(input);
    assert.equal(Buffer.from(canonical).toString('utf8'), v.canonical);
    assert.equal(digestContext(v.kind), v.digest_context);
    assert.equal(digest(v.kind, canonical), v.digest);
  });
}

for (const v of vectors.payload_negative) {
  test(`payload negative ${v.name}`, () => {
    assert.equal(code(() => validate(v.kind as Kind, bytes(v.input_b64))), v.expected_code);
  });
}

for (const v of vectors.jws_positive) {
  test(`jws positive ${v.name}`, async () => {
    assert.equal(protectedHeader(v.kind as SignedKind, v.kid), v.protected_header);
    const payload = bytes(v.jws.split('.')[1]);
    assert.equal(signingInput(v.kind as SignedKind, v.kid, payload), v.signing_input);
    const res = await verify(v.jws, v.kind as SignedKind, trust);
    assert.equal(res.digest, v.payload_digest);
    assert.equal(res.kid, v.kid);
  });
}

for (const v of vectors.jws_negative) {
  test(`jws negative ${v.name}`, async () => {
    assert.equal(await codeAsync(() => verify(v.jws, v.kind as SignedKind, trust)), v.expected_code);
  });
}
