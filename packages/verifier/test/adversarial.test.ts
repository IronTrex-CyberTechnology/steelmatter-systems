import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { loadTrustStore, parseTimestamp, verifyBundle } from '../src/index.ts';

const vectors = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/evidence-v1.json', import.meta.url), 'utf8'));
const complete = vectors.cases.find((c: any) => c.name === 'complete').bundle;
const trust = await loadTrustStore(vectors.trust_store);
const now = parseTimestamp(vectors.usable_at)!;
const kinds = ['mandate', 'permit', 'execution', 'observation'];
function clean(report: Awaited<ReturnType<typeof verifyBundle>>) {
  return Object.values(report.records).every(r => r.integrity === 'VALID' && r.issuer === 'TRUSTED' && r.payload === 'VALID' && r.profile === 'SUPPORTED')
    && report.action.payload === 'VALID' && report.linkage.status === 'COMPLETE'
    && report.chronology.status === 'CONSISTENT' && report.observation.observed_matches_action === true;
}

test('adversarial evidence: signature, header, payload and record-type mutations cannot verify cleanly', async t => {
  assert.equal(clean(await verifyBundle(complete, trust, now)), true, 'valid control');
  let checked = 0;
  for (const kind of kinds) {
    const original = complete.records[`${kind}_jws`] as string;
    const [h, p, s] = original.split('.') as [string, string, string];
    const signature = Buffer.from(s, 'base64url');
    const changes: string[] = ['', `${original}.extra`, `${h}.${p}.`, `${h}.${p}.${s}=`, `${h}.${p}.${s}\n`];
    for (const offset of [0, 15, 31, 47, 63]) {
      const changed = Buffer.from(signature); changed[offset] = changed[offset]! ^ 1;
      changes.push(`${h}.${p}.${changed.toString('base64url')}`);
    }
    const header = JSON.parse(Buffer.from(h, 'base64url').toString());
    for (const changed of [{...header, alg: 'none'}, {...header, alg: 'HS256'}, {...header, jku: 'https://synthetic.invalid/keys'}, {...header, kid: 'untrusted'}]) {
      changes.push(`${Buffer.from(JSON.stringify(changed)).toString('base64url')}.${p}.${s}`);
    }
    const payload = JSON.parse(Buffer.from(p, 'base64url').toString());
    changes.push(`${h}.${Buffer.from(JSON.stringify({...payload, tenant_id: 'other-org'})).toString('base64url')}.${s}`);
    for (const other of kinds.filter(k => k !== kind)) changes.push(complete.records[`${other}_jws`]);
    for (const changed of changes) {
      const bundle = structuredClone(complete);
      bundle.records[`${kind}_jws`] = changed;
      bundle.evidence = 'AVAILABLE'; bundle.incomplete = []; // false service claims cannot rescue a broken chain
      assert.equal(clean(await verifyBundle(bundle, trust, now)), false, `${kind} mutation ${checked}`);
      checked++;
    }
  }
  t.diagnostic(`${checked} signature/header/payload/type mutations failed the clean-evidence predicate`);
});

test('adversarial evidence: every record-removal combination remains incomplete', async t => {
  for (let mask = 1; mask < 16; mask++) {
    const bundle = structuredClone(complete);
    for (let i = 0; i < kinds.length; i++) if (mask & (1 << i)) delete bundle.records[`${kinds[i]}_jws`];
    bundle.evidence = 'AVAILABLE'; bundle.incomplete = [];
    const report = await verifyBundle(bundle, trust, now);
    assert.equal(report.linkage.status, 'INCOMPLETE', `mask ${mask}`);
    assert.equal(clean(report), false);
  }
  t.diagnostic('15 missing-record combinations rejected as complete evidence');
});

test('adversarial evidence: every claimed link is checked against verified content', async t => {
  for (const key of Object.keys(complete.links)) {
    const bundle = structuredClone(complete);
    bundle.links[key] = key.endsWith('_digest') ? '0'.repeat(64) : 'forged-link';
    const report = await verifyBundle(bundle, trust, now);
    assert.equal(report.linkage.status, 'BROKEN', key);
    assert.equal(clean(report), false, key);
  }
  t.diagnostic(`${Object.keys(complete.links).length} false link claims detected`);
});
