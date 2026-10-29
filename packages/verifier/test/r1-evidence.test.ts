import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { generateKeyPair, exportJWK, calculateJwkThumbprint } from 'jose';
import { verifyBundle, loadTrustStore, sign, digest, canonicalBytes, validate, type SignedKind } from '../src/index.ts';

const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/evidence-v1.json', import.meta.url), 'utf8'));
const original = fx.cases.find((c: any) => c.name === 'complete').bundle;
const pair = await generateKeyPair('ES256', { extractable: true });
const jwk = await exportJWK(pair.publicKey);
const kid = await calculateJwkThumbprint(jwk, 'sha256');
const trust = await loadTrustStore({ trust_store_version: '1', keys: [{ kid, name: 'R1-test', alg: 'ES256', synthetic: true,
  purposes: ['SM-MANDATE+JWS', 'SM-PERMIT+JWS', 'SM-EXECUTION+JWS', 'SM-OBSERVATION+JWS'], created_at: '2026-01-01T00:00:00Z',
  jwk: { kty: 'EC', crv: 'P-256', x: jwk.x, y: jwk.y } }] } as any);
const bytes = (p: any) => new TextEncoder().encode(JSON.stringify(p));
const date = (s: string, seconds: number) => new Date(Date.parse(s) + seconds * 1000).toISOString().replace('.000Z', 'Z');
async function bundle(alter?: (p: any) => void) {
  const p: any = {};
  for (const kind of ['mandate', 'permit', 'execution', 'observation']) p[kind] = JSON.parse(Buffer.from(original.records[`${kind}_jws`].split('.')[1], 'base64url').toString());
  p.execution.schema_version = '2';
  p.execution.admitted_at = p.execution.committed_at;
  p.execution.committed_at = date(p.permit.expires_at, 60);
  p.execution.company_ref = 'acme-in';
  p.execution.target_order_id = 'PO/2026/001';
  p.observation.schema_version = '2';
  p.observation.observed_at = date(p.execution.committed_at, 1);
  p.observation.target_order_id = p.execution.target_order_id;
  p.observation.expected_company_ref = p.observation.observed_company_ref = 'acme-in';
  p.observation.observed_state = 'purchase';
  p.observation.expected_permit_digest = p.observation.observed_permit_digest = p.execution.permit_digest;
  alter?.(p);
  // Re-link after changing timestamps; these are authentic chains, not corrupt signatures.
  p.observation.execution_digest = digest('execution', canonicalBytes(p.execution));
  const records: any = {};
  for (const kind of ['mandate', 'permit', 'execution', 'observation'] as SignedKind[]) records[`${kind}_jws`] = await sign(kind, bytes(p[kind]), kid, pair.privateKey);
  return { operation_id: p.execution.operation_id, action: original.action, records };
}

test('R1: late confirmation preserves valid admission and verifies a complete signed chain', async () => {
  const r = await verifyBundle(await bundle(), trust);
  assert.equal(r.linkage.status, 'COMPLETE');
  assert.equal(r.chronology.status, 'CONSISTENT');
  assert.equal(r.observation.status, 'MATCH');
  assert.equal(r.chronology.checks.length, 6);
});
test('R1: admission at expiry breaks chronology even with a valid signature', async () => {
  const r = await verifyBundle(await bundle(p => { p.execution.admitted_at = p.permit.expires_at; }), trust);
  assert.equal(r.chronology.status, 'INCONSISTENT');
  assert.equal(r.linkage.status, 'BROKEN');
});
test('R1: a signed observation for another company cannot attest this execution', async () => {
  const r = await verifyBundle(await bundle(p => { p.observation.expected_company_ref = p.observation.observed_company_ref = 'other'; }), trust);
  assert.equal(r.linkage.status, 'BROKEN');
  assert.ok(r.linkage.checks.some(c => c.name === 'destination_binding' && c.status === 'MISMATCH'));
});
test('R1: a v1 observation cannot silently downgrade destination verification', async () => {
  const b = await bundle(p => {
    p.execution.target_order_id = p.observation.target_order_id = 'po-1';
    p.observation.schema_version = '1';
    for (const k of ['expected_company_ref', 'observed_company_ref', 'observed_state', 'expected_permit_digest', 'observed_permit_digest']) delete p.observation[k];
  });
  const r = await verifyBundle(b, trust);
  assert.equal(r.linkage.status, 'BROKEN');
});
test('R1: destination mismatch remains authentic evidence, never MATCH', async () => {
  for (const field of ['observed_company_ref', 'observed_state', 'observed_permit_digest']) {
    const r = await verifyBundle(await bundle(p => { p.observation[field] = 'changed'; p.observation.status = 'MISMATCH'; }), trust);
    assert.equal(r.linkage.status, 'COMPLETE', field);
    assert.equal(r.observation.status, 'MISMATCH', field);
  }
});
test('R1: required admission and destination fields cannot be omitted or marked v1', async () => {
  const b = await bundle();
  for (const kind of ['execution', 'observation'] as const) {
    const payload = JSON.parse(Buffer.from(b.records[`${kind}_jws`].split('.')[1], 'base64url').toString());
    payload.schema_version = '1';
    assert.throws(() => validate(kind, bytes(payload)));
    payload.schema_version = '2';
    delete payload[kind === 'execution' ? 'admitted_at' : 'observed_company_ref'];
    assert.throws(() => validate(kind, bytes(payload)));
  }
});
