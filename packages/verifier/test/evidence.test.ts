// Evidence bundle verification against the Go-generated chain
// (fixtures/vectors/evidence-v1.json): T21/T26 verifier portion.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { generateKeyPair, exportJWK, calculateJwkThumbprint } from 'jose';
import { verifyBundle, loadTrustStore, parseTimestamp } from '../src/index.ts';

const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/evidence-v1.json', import.meta.url), 'utf8'));
const trust = await loadTrustStore(fx.trust_store);
const usableAt = parseTimestamp(fx.usable_at)!;
const clone = (v: unknown) => JSON.parse(JSON.stringify(v));
const byName = (n: string) => fx.cases.find((c: any) => c.name === n);

for (const c of fx.cases) {
  test(`fixture ${c.name}`, async () => {
    const r = await verifyBundle(c.bundle, trust, usableAt);
    assert.equal(r.linkage.status, c.expect.linkage);
    if (c.expect.observation) assert.equal(r.observation.status, c.expect.observation);
    if ('observed_matches_action' in c.expect) assert.equal(r.observation.observed_matches_action, c.expect.observed_matches_action);
    if (c.expect.broken_check) {
      assert.deepEqual(r.linkage.checks.filter((x) => x.status === 'MISMATCH').map((x) => x.name), [c.expect.broken_check]);
    }
    for (const [kind, rec] of Object.entries(r.records)) {
      if (rec.integrity !== 'MISSING') assert.deepEqual([rec.integrity, rec.issuer, rec.profile, rec.payload], ['VALID', 'TRUSTED', 'SUPPORTED', 'VALID'], kind);
    }
  });
}

test('report has no single overall verdict', async () => {
  const r: any = await verifyBundle(byName('complete').bundle, trust, usableAt);
  for (const k of ['valid', 'safe', 'ok', 'verdict', 'verified', 'passed']) assert.equal(k in r, false, k);
});

test('historical permit: authentic evidence, expired for use', async () => {
  const r = await verifyBundle(byName('complete').bundle, trust); // real current time
  assert.equal(r.records.permit.integrity, 'VALID');
  assert.equal(r.usability.permit, 'EXPIRED');
  assert.equal(r.linkage.status, 'COMPLETE');
});

test('T26: changing a signed field in any record breaks its integrity and leaves linkage incomplete', async () => {
  for (const kind of ['mandate', 'permit', 'execution', 'observation']) {
    const b = clone(byName('complete').bundle);
    const [h, p, s] = b.records[`${kind}_jws`].split('.');
    const payload = JSON.parse(Buffer.from(p, 'base64url').toString());
    payload.tenant_id = 'forged-tenant';
    const keys = Object.keys(payload).sort();
    const canonical = JSON.stringify(Object.fromEntries(keys.map((k) => [k, payload[k]])));
    b.records[`${kind}_jws`] = `${h}.${Buffer.from(canonical).toString('base64url')}.${s}`;
    const r = await verifyBundle(b, trust, usableAt);
    assert.equal(r.records[kind as 'permit'].integrity, 'INVALID', kind);
    assert.equal(r.records[kind as 'permit'].code, 'JWS_SIGNATURE_INVALID', kind);
    assert.notEqual(r.linkage.status, 'COMPLETE', kind);
  }
});

test('T26: removing required chain records is reported as incomplete, not valid', async () => {
  for (const kind of ['mandate', 'permit', 'execution', 'observation']) {
    const b = clone(byName('complete').bundle);
    delete b.records[`${kind}_jws`];
    const r = await verifyBundle(b, trust, usableAt);
    assert.equal(r.records[kind as 'permit'].integrity, 'MISSING');
    assert.equal(r.linkage.status, 'INCOMPLETE', kind);
  }
  const noAction = clone(byName('complete').bundle);
  delete noAction.action;
  const r = await verifyBundle(noAction, trust, usableAt);
  assert.equal(r.action.payload, 'MISSING');
  assert.equal(r.linkage.status, 'INCOMPLETE');
});

test('T26: removing version data from a signed payload is rejected', async () => {
  const b = clone(byName('complete').bundle);
  const [h, p, s] = b.records.execution_jws.split('.');
  const payload = JSON.parse(Buffer.from(p, 'base64url').toString());
  delete payload.schema_version;
  b.records.execution_jws = `${h}.${Buffer.from(JSON.stringify(payload)).toString('base64url')}.${s}`;
  const r = await verifyBundle(b, trust, usableAt);
  assert.equal(r.records.execution.integrity, 'INVALID');
});

test('tampered action breaks linkage', async () => {
  const b = clone(byName('complete').bundle);
  b.action.delivery_id = 'warehouse-z';
  const r = await verifyBundle(b, trust, usableAt);
  assert.equal(r.linkage.status, 'BROKEN');
  assert.ok(r.linkage.checks.some((c) => c.name === 'action_digest:permit' && c.status === 'MISMATCH'));
});

test('false service link claims are detected, not trusted', async () => {
  const b = clone(byName('complete').bundle);
  b.links.permit_digest = 'a'.repeat(64);
  const r = await verifyBundle(b, trust, usableAt);
  assert.equal(r.linkage.status, 'BROKEN');
  assert.ok(r.linkage.checks.some((c) => c.name === 'service_link_claims' && c.status === 'MISMATCH'));
  b.evidence = 'AVAILABLE';
  assert.equal(r.service_claims.evidence, 'AVAILABLE'); // reported only
});

test('explicit trust anchors: a different key leaves every record untrusted and unverified', async () => {
  const { publicKey } = await generateKeyPair('ES256', { extractable: true });
  const jwk = await exportJWK(publicKey);
  const kid = await calculateJwkThumbprint(jwk, 'sha256');
  const other = await loadTrustStore({ trust_store_version: '1', keys: [{ kid, name: 'other', alg: 'ES256', synthetic: true,
    purposes: ['SM-MANDATE+JWS', 'SM-PERMIT+JWS', 'SM-EXECUTION+JWS', 'SM-OBSERVATION+JWS'], created_at: '2026-10-12T00:00:00Z',
    jwk: { kty: 'EC', crv: 'P-256', x: jwk.x, y: jwk.y } }] } as any);
  const r = await verifyBundle(byName('complete').bundle, other, usableAt);
  for (const rec of Object.values(r.records)) assert.deepEqual([rec.integrity, rec.issuer], ['UNVERIFIED', 'UNTRUSTED']);
  assert.equal(r.observation.status, 'UNVERIFIED');
});

function cli(bundle: unknown, extra: string[] = []) {
  const dir = mkdtempSync(join(tmpdir(), 'smv-'));
  writeFileSync(join(dir, 't.json'), JSON.stringify(fx.trust_store));
  writeFileSync(join(dir, 'b.json'), JSON.stringify(bundle));
  return spawnSync(process.execPath, [fileURLToPath(new URL('../src/cli.ts', import.meta.url)), 'verify', '--trust', join(dir, 't.json'), '--bundle', join(dir, 'b.json'), ...extra], { encoding: 'utf8' });
}

test('CLI: report produced (exit 0) without --fail-on, even for broken evidence', () => {
  const r = cli(byName('broken-permit-link').bundle, ['--format', 'json', '--now', fx.usable_at]);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(JSON.parse(r.stdout).linkage.status, 'BROKEN');
});

test('CLI: --fail-on makes named dimensions gate the exit code', () => {
  assert.equal(cli(byName('complete').bundle, ['--fail-on', 'integrity,issuer,profile,linkage,observation', '--now', fx.usable_at]).status, 0);
  assert.equal(cli(byName('broken-permit-link').bundle, ['--fail-on', 'linkage']).status, 1);
  assert.equal(cli(byName('observer-mismatch').bundle, ['--fail-on', 'observation']).status, 1);
  assert.equal(cli(byName('complete').bundle, ['--fail-on', 'usability']).status, 1); // historical permit
});

test('CLI: missing trust anchors is a usage error (no remote fetch)', () => {
  const r = spawnSync(process.execPath, [fileURLToPath(new URL('../src/cli.ts', import.meta.url)), 'verify', '--bundle', 'x.json'], { encoding: 'utf8' });
  assert.equal(r.status, 2);
  assert.match(r.stderr, /never fetched/);
});

test('verifier source has no network access paths', () => {
  const dir = new URL('../src/', import.meta.url);
  for (const f of readdirSync(dir)) {
    const src = readFileSync(new URL(f, dir), 'utf8');
    for (const banned of ['node:http', 'node:https', 'node:net', 'node:dgram', 'fetch(', 'XMLHttpRequest', 'WebSocket', 'createRemoteJWKSet']) {
      assert.equal(src.includes(banned), false, `${f} uses ${banned}`);
    }
  }
});

function retiredTrust(notAfter: string) {
  const ts = clone(fx.trust_store);
  // The fixture records are dated 2026-10-09 (packet examples); date the key's
  // creation before them so a retirement on that day is valid.
  ts.keys[0].created_at = '2026-10-01T00:00:00Z';
  ts.keys[0].not_after = notAfter;
  return loadTrustStore(ts);
}

test('historical key: records signed before retirement stay authentic evidence', async () => {
  const t = await retiredTrust('2026-10-09T12:00:00Z'); // after every record time, before "now"
  const r = await verifyBundle(byName('complete').bundle, t);
  for (const [kind, rec] of Object.entries(r.records)) {
    assert.deepEqual([rec.integrity, rec.issuer, rec.key_status, rec.signed_within_key_validity], ['VALID', 'TRUSTED', 'RETIRED', true], kind);
  }
  assert.equal(r.linkage.status, 'COMPLETE');
});

test('historical key: a record dated at or after its key retirement is untrusted', async () => {
  const t = await retiredTrust('2026-10-09T10:01:30Z'); // permit 10:01 is before it; execution 10:02 and observation 10:03 are after
  const r = await verifyBundle(byName('complete').bundle, t, usableAt);
  assert.equal(r.records.permit.issuer, 'TRUSTED');
  for (const kind of ['execution', 'observation'] as const) {
    assert.equal(r.records[kind].issuer, 'UNTRUSTED', kind);
    assert.equal(r.records[kind].signed_within_key_validity, false, kind);
  }
  assert.notEqual(r.linkage.status, 'COMPLETE');
});

test('key without not_after reports CURRENT and no validity window', async () => {
  const r = await verifyBundle(byName('complete').bundle, trust, usableAt);
  assert.equal(r.records.permit.key_status, 'CURRENT');
  assert.equal(r.records.permit.signed_within_key_validity, null);
});
