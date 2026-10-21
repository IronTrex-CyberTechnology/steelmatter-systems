// ADR-003 parity: cross-record timestamp checks against the Go-generated
// fixture (fixtures/vectors/evidence-temporal-v1.json). Every record is
// authentic and every digest link holds; only timestamps differ. The verifier
// must report exactly the expected broken time checks, an explicit
// chronology result, and the CLI gates must catch every impossible chain.
// tests/conformance runs the same file through Go.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { verifyBundle, loadTrustStore, parseTimestamp, REPORT_VERSION } from '../src/index.ts';

const fx = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/evidence-temporal-v1.json', import.meta.url), 'utf8'));
const trust = await loadTrustStore(fx.trust_store);
const cli = fileURLToPath(new URL('../src/cli.ts', import.meta.url));
const TIME_CHECKS = ['time:permit_issued_within_mandate', 'time:permit_expiry_within_mandate', 'time:execution_within_permit',
  'time:execution_within_mandate', 'time:observation_after_execution'];
const SCENARIOS = ['S1', 'S2', 'S3', 'S4', 'S5', 'S6'];

test('temporal fixture covers the six reported scenarios, every rule and the valid controls', () => {
  for (const s of SCENARIOS) {
    const c = fx.cases.filter((x: any) => x.covers.startsWith(`${s} `));
    assert.equal(c.length, 1, `${s} has exactly one case`);
    assert.equal(c[0].valid, false);
  }
  const broken = new Set(fx.cases.flatMap((c: any) => c.expect.broken_checks));
  assert.deepEqual([...broken].sort(), [...TIME_CHECKS].sort());
  for (const name of ['consistent', 'observation-same-second-as-execution', 'execution-last-second-before-permit-expiry', 'boundaries-equal', 'pending-without-execution']) {
    assert.ok(fx.cases.some((c: any) => c.name === name && c.valid), `valid control ${name}`);
  }
});

for (const c of fx.cases) {
  test(`temporal ${c.name}`, async () => {
    const r = await verifyBundle(c.bundle, trust);
    assert.equal(r.report_version, REPORT_VERSION);
    assert.equal(REPORT_VERSION, '2');
    for (const [kind, rec] of Object.entries(r.records)) {
      if (rec.integrity !== 'MISSING') assert.deepEqual([rec.integrity, rec.issuer, rec.payload], ['VALID', 'TRUSTED', 'VALID'], `${kind} is authentic`);
    }
    const mismatched = r.linkage.checks.filter((x) => x.status === 'MISMATCH').map((x) => x.name);
    assert.deepEqual(mismatched, c.expect.broken_checks, 'exactly the expected time checks fail, and no other link');
    assert.equal(r.linkage.status, c.expect.linkage);
    // The explicit chronology result carries exactly the time checks.
    assert.equal(r.chronology.status, c.expect.chronology);
    assert.deepEqual(r.chronology.checks.map((x) => x.name), TIME_CHECKS);
    assert.deepEqual(r.chronology.checks.filter((x) => x.status === 'MISMATCH').map((x) => x.name), c.expect.broken_checks);
    for (const x of r.chronology.checks.filter((y) => y.status === 'MISMATCH')) assert.ok(x.detail, `${x.name} explains itself`);
  });
}

test('historical evidence: valid chains stay CONSISTENT long after their permits expired', async () => {
  for (const c of fx.cases.filter((x: any) => x.valid && x.expect.linkage === 'COMPLETE')) {
    const permitExpiry = parseTimestamp(JSON.parse(Buffer.from(c.bundle.records.permit_jws.split('.')[1], 'base64url').toString()).expires_at)!;
    const r = await verifyBundle(c.bundle, trust, permitExpiry + 365 * 24 * 3600);
    assert.equal(r.usability.permit, 'EXPIRED', c.name);
    assert.equal(r.chronology.status, 'CONSISTENT', c.name);
    assert.equal(r.linkage.status, 'COMPLETE', c.name);
  }
});

function runCli(c: any, failOn: string, now?: string) {
  const dir = mkdtempSync(join(tmpdir(), 'smv-temporal-'));
  writeFileSync(join(dir, 't.json'), JSON.stringify(fx.trust_store));
  writeFileSync(join(dir, 'b.json'), JSON.stringify(c.bundle));
  const args = [cli, 'verify', '--trust', join(dir, 't.json'), '--bundle', join(dir, 'b.json'), '--fail-on', failOn, ...(now ? ['--now', now] : [])];
  const r = spawnSync(process.execPath, args, { encoding: 'utf8' });
  return { status: r.status, stdout: String(r.stdout), stderr: String(r.stderr) };
}

test('CLI: --fail-on chronology and --fail-on linkage reject every impossible chain and pass the valid controls', () => {
  for (const c of fx.cases) {
    const chron = runCli(c, 'chronology');
    const link = runCli(c, 'linkage');
    if (!c.valid) {
      assert.equal(chron.status, 1, `${c.name}: chronology gate`);
      assert.equal(link.status, 1, `${c.name}: linkage gate`);
      assert.match(chron.stdout, /chronology: INCONSISTENT/, c.name);
      for (const name of c.expect.broken_checks) assert.ok(chron.stdout.includes(`MISMATCH ${name}`), `${c.name}: names ${name}`);
      assert.match(chron.stderr, /--fail-on dimensions not clean: chronology/, c.name);
    } else if (c.expect.linkage === 'COMPLETE') {
      assert.equal(chron.status, 0, `${c.name}: ${chron.stdout}${chron.stderr}`);
      assert.equal(link.status, 0, `${c.name}: ${link.stdout}${link.stderr}`);
      assert.match(chron.stdout, /chronology: CONSISTENT/, c.name);
    } else {
      // A pending chain is not inconsistent, but it is not clean either.
      assert.match(chron.stdout, /chronology: INCOMPLETE/, c.name);
      assert.equal(chron.status, 1, c.name);
    }
  }
});

test('CLI: historical valid evidence passes every evidence gate while usability alone reports the expired permit', () => {
  const c = fx.cases.find((x: any) => x.name === 'execution-last-second-before-permit-expiry');
  const later = '2030-01-01T00:00:00Z';
  assert.equal(runCli(c, 'integrity,issuer,profile,linkage,chronology,observation', later).status, 0);
  const u = runCli(c, 'usability', later);
  assert.equal(u.status, 1);
  assert.match(u.stdout, /usability: permit=EXPIRED/);
});
