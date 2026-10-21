// The verifier is only offered in the browser because it is tested there:
// the browser bundle (the verifier source plus the browser stand-ins in
// src/shims) must give exactly the same result as the Node verifier for every
// golden vector and every evidence chain (document 03: browser support is
// optional until tested).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import * as node from '../../packages/verifier/src/index.ts';
import { buildVerifierBundle, tempDir } from './helpers.ts';

const browser: typeof node = await import(pathToFileURL(await buildVerifierBundle(join(tempDir('verifier'), 'verifier-browser.mjs'))).href);
const read = (f: string) => JSON.parse(readFileSync(new URL(`../../fixtures/vectors/${f}`, import.meta.url), 'utf8'));
const golden = read('golden-v1.json');
const evidence = read('evidence-v1.json');
const bytes = (b64: string) => new Uint8Array(Buffer.from(b64, 'base64url'));

async function outcome(fn: () => unknown): Promise<string> {
  try {
    return JSON.stringify({ ok: await fn() });
  } catch (e) {
    return JSON.stringify({ code: (e as { code?: string }).code ?? String(e) });
  }
}

test('browser bundle verifies every golden JWS vector exactly like the Node verifier', async () => {
  const tb = await browser.loadTrustStore(golden.trust_store);
  const tn = await node.loadTrustStore(golden.trust_store);
  const all = [...golden.jws_positive, ...golden.jws_negative];
  assert.ok(all.length > 30);
  for (const v of all) {
    const b = await outcome(() => browser.verify(v.jws, v.kind, tb));
    assert.equal(b, await outcome(() => node.verify(v.jws, v.kind, tn)), v.name);
    if (v.expected_code) assert.equal(JSON.parse(b).code, v.expected_code, v.name);
    else assert.equal(JSON.parse(b).ok.digest, v.payload_digest, v.name);
  }
});

test('browser bundle validates, canonicalises and digests every golden payload vector exactly like the Node verifier', async () => {
  for (const v of [...golden.canonical, ...golden.payload_negative]) {
    const run = (m: typeof node) => outcome(() => {
      m.validate(v.kind, bytes(v.input_b64));
      const c = m.canonicalizeRaw(bytes(v.input_b64));
      return { canonical: Buffer.from(c).toString('utf8'), digest: m.digest(v.kind, c) };
    });
    const b = await run(browser);
    assert.equal(b, await run(node), v.name);
    if (v.expected_code) assert.equal(JSON.parse(b).code, v.expected_code, v.name);
    else assert.deepEqual(JSON.parse(b).ok, { canonical: v.canonical, digest: v.digest }, v.name);
  }
});

test('browser bundle reports every evidence chain exactly like the Node verifier, inside and after the permit window', async () => {
  const tb = await browser.loadTrustStore(evidence.trust_store);
  const tn = await node.loadTrustStore(evidence.trust_store);
  const inside = Math.floor(Date.parse(evidence.usable_at) / 1000);
  const after = inside + 365 * 24 * 3600;
  for (const c of evidence.cases) {
    for (const at of [inside, after]) {
      const b = await browser.verifyBundle(c.bundle, tb, at);
      assert.deepEqual(b, await node.verifyBundle(c.bundle, tn, at), `${c.name} at ${at}`);
    }
    const r = await browser.verifyBundle(c.bundle, tb, inside);
    for (const [k, want] of Object.entries(c.expect)) {
      const got = k === 'broken_check' ? r.linkage.checks.find((x) => x.status !== 'OK')?.name
        : k === 'linkage' ? r.linkage.status : k === 'observation' ? r.observation.status : r.observation.observed_matches_action;
      assert.equal(got, want, `${c.name} ${k}`);
    }
  }
});

test('browser bundle reports every chronology case (ADR-003) exactly like the Node verifier, and the CLI gates agree', async () => {
  const temporal = read('evidence-temporal-v1.json');
  const tb = await browser.loadTrustStore(temporal.trust_store);
  const tn = await node.loadTrustStore(temporal.trust_store);
  assert.ok(temporal.cases.length >= 13);
  for (const c of temporal.cases) {
    const b = await browser.verifyBundle(c.bundle, tb);
    assert.deepEqual(b, await node.verifyBundle(c.bundle, tn), c.name);
    assert.equal(b.chronology.status, c.expect.chronology, c.name);
    assert.deepEqual(b.chronology.checks.filter((x) => x.status === 'MISMATCH').map((x) => x.name), c.expect.broken_checks, c.name);
  }
});

test('historical permit: authentic evidence in the browser, expired for use', async () => {
  const t = await browser.loadTrustStore(evidence.trust_store);
  const complete = evidence.cases.find((c: { name: string }) => c.name === 'complete');
  const r = await browser.verifyBundle(complete.bundle, t);
  assert.equal(r.records.permit.integrity, 'VALID');
  assert.equal(r.usability.permit, 'EXPIRED');
});

test('the browser bundle has no network access paths', () => {
  const src = readFileSync(new URL('../src/shims/crypto.ts', import.meta.url), 'utf8')
    + readFileSync(new URL('../src/shims/fs.ts', import.meta.url), 'utf8')
    + readFileSync(new URL('../src/shims/module.ts', import.meta.url), 'utf8')
    + readFileSync(new URL('../src/shims/buffer.ts', import.meta.url), 'utf8');
  for (const banned of ['fetch(', 'XMLHttpRequest', 'WebSocket', 'node:http', 'node:net']) assert.equal(src.includes(banned), false, banned);
});
