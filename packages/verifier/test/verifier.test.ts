import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { generateKeyPair, exportJWK, calculateJwkThumbprint } from 'jose';
import { sign, verify, loadTrustStore, parseStrict, parseTimestamp, ContractError } from '../src/index.ts';

const enc = new TextEncoder();
const permit = readFileSync(new URL('../../../fixtures/packet/permit.payload.json', import.meta.url));

async function freshKey(purposes: string[]) {
  const { privateKey, publicKey } = await generateKeyPair('ES256', { extractable: true });
  const jwk = await exportJWK(publicKey);
  const kid = await calculateJwkThumbprint(jwk, 'sha256');
  const entry = { kid, name: 'ts-fresh', alg: 'ES256', synthetic: true, purposes, created_at: '2026-10-12T00:00:00Z',
    jwk: { kty: 'EC', crv: 'P-256', x: jwk.x, y: jwk.y } };
  return { privateKey, kid, trust: await loadTrustStore({ trust_store_version: '1', keys: [entry] } as any) };
}

test('fresh TypeScript signature round-trips', async () => {
  const { privateKey, kid, trust } = await freshKey(['SM-PERMIT+JWS']);
  const jws = await sign('permit', new Uint8Array(permit), kid, privateKey);
  const v = await verify(jws, 'permit', trust);
  assert.equal((v.value as any).amount_minor, 60000000);
});

test('fresh signatures differ but both verify (no byte-equality requirement)', async () => {
  const { privateKey, kid, trust } = await freshKey(['SM-PERMIT+JWS']);
  const a = await sign('permit', new Uint8Array(permit), kid, privateKey);
  const b = await sign('permit', new Uint8Array(permit), kid, privateKey);
  assert.notEqual(a, b);
  await verify(a, 'permit', trust);
  await verify(b, 'permit', trust);
});

test('trust store rejects kid that is not the thumbprint', async () => {
  const { trust } = await freshKey(['SM-PERMIT+JWS']);
  const k = { ...trust.keys[0], kid: 'not-the-thumbprint' };
  await assert.rejects(loadTrustStore({ trust_store_version: '1', keys: [k] } as any));
});

test('trust store rejects unknown fields such as a remote key URL', async () => {
  await assert.rejects(loadTrustStore({ trust_store_version: '1', keys: [], jku: 'https://attacker.invalid' } as any));
});

test('parser: __proto__ is an ordinary key and cannot pollute prototypes', () => {
  const v: any = parseStrict(enc.encode('{"__proto__":{"polluted":true}}'));
  assert.equal(({} as any).polluted, undefined);
  assert.deepEqual(Object.keys(v), ['__proto__']);
});

test('parser: payload over 1 MiB is rejected', () => {
  const big = enc.encode(`{"a":"${'x'.repeat(1 << 20)}"}`);
  assert.throws(() => parseStrict(big), (e: unknown) => e instanceof ContractError && e.code === 'PAYLOAD_TOO_LARGE');
});

test('timestamps: real calendar, whole seconds, Z only', () => {
  assert.notEqual(parseTimestamp('2028-02-29T23:59:59Z'), null);
  for (const bad of ['2026-02-29T10:00:00Z', '2026-12-31T23:59:60Z', '2026-10-09T10:00:00.5Z', '2026-10-09T10:00:00+05:30', '2026-10-09T24:00:00Z']) {
    assert.equal(parseTimestamp(bad), null, bad);
  }
});
