#!/usr/bin/env node
// steelmatter-verify: offline verifier for Steelmatter evidence. It makes no
// network calls and needs no engine, database or model API.
//
//   steelmatter-verify verify --trust TRUST_STORE.json --bundle EVIDENCE.json [--now RFC3339] [--format text|json]
//                             [--fail-on integrity,issuer,profile,linkage,chronology,observation,usability]
//     Prints a report with each dimension separate. Exit 0 once a report is
//     produced, unless --fail-on names dimensions that must be clean (then 1).
//     Exit 2 for usage or input errors.
//
//   steelmatter-verify cross IN OUT   (contract tooling for the Go/TypeScript conformance suite)
import { readFileSync, writeFileSync } from 'node:fs';
import { generateKeyPair, exportJWK, calculateJwkThumbprint } from 'jose';
import { verify, sign, signingInput, loadTrustStore, parseStrict, parseTimestamp, type SignedKind } from './index.ts';
import { verifyBundle, type EvidenceReport } from './evidence.ts';

function usage(msg?: string): never {
  if (msg) console.error(`steelmatter-verify: ${msg}`);
  console.error('usage: steelmatter-verify verify --trust FILE --bundle FILE [--now RFC3339] [--format text|json] [--fail-on LIST]\n  --fail-on dimensions: integrity,issuer,profile,linkage,chronology,observation,usability');
  process.exit(2);
}

function flags(args: string[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (let i = 0; i < args.length; i += 2) {
    if (!args[i]?.startsWith('--') || args[i + 1] === undefined) usage(`bad argument ${args[i] ?? ''}`);
    out[args[i].slice(2)] = args[i + 1];
  }
  return out;
}

export function failures(r: EvidenceReport, dims: string[]): string[] {
  const recs = Object.entries(r.records);
  const bad: string[] = [];
  for (const d of dims) {
    if (d === 'integrity' && recs.some(([, x]) => x.integrity !== 'VALID')) bad.push(d);
    else if (d === 'issuer' && recs.some(([, x]) => x.issuer !== 'TRUSTED')) bad.push(d);
    else if (d === 'profile' && (recs.some(([, x]) => x.profile !== 'SUPPORTED' || x.payload !== 'VALID') || r.action.payload !== 'VALID')) bad.push(d);
    else if (d === 'linkage' && r.linkage.status !== 'COMPLETE') bad.push(d);
    else if (d === 'chronology' && r.chronology.status !== 'CONSISTENT') bad.push(d);
    else if (d === 'observation' && (r.observation.status !== 'MATCH' || r.observation.observed_matches_action !== true)) bad.push(d);
    else if (d === 'usability' && r.usability.permit !== 'USABLE') bad.push(d);
    else if (!['integrity', 'issuer', 'profile', 'linkage', 'chronology', 'observation', 'usability'].includes(d)) usage(`unknown --fail-on dimension ${d}`);
  }
  return bad;
}

function text(r: EvidenceReport): string {
  const lines = [`operation ${r.operation_id ?? '(none)'} evaluated at ${r.evaluated_at}`, '', 'records:'];
  for (const [k, x] of Object.entries(r.records)) {
    lines.push(`  ${k.padEnd(12)} integrity=${x.integrity} issuer=${x.issuer} profile=${x.profile} payload=${x.payload}${x.code ? ` (${x.code})` : ''}`);
  }
  lines.push(`  ${'action'.padEnd(12)} payload=${r.action.payload}${r.action.digest ? ` digest=${r.action.digest}` : ''}`);
  lines.push('', `usability: permit=${r.usability.permit} mandate=${r.usability.mandate}`, `  ${r.usability.note}`);
  lines.push('', `linkage: ${r.linkage.status}`);
  for (const c of r.linkage.checks.filter((c) => c.status !== 'OK')) lines.push(`  ${c.status.padEnd(8)} ${c.name}${c.detail ? ` (${c.detail})` : ''}`);
  lines.push('', `chronology: ${r.chronology.status}`);
  for (const c of r.chronology.checks.filter((c) => c.status !== 'OK')) lines.push(`  ${c.status.padEnd(8)} ${c.name}${c.detail ? ` (${c.detail})` : ''}`);
  lines.push('', `observation: ${r.observation.status} observed_matches_action=${r.observation.observed_matches_action}`);
  lines.push('', 'No overall verdict is given; each line above is a separate result.');
  return lines.join('\n');
}

async function verifyCmd(args: string[]): Promise<void> {
  const f = flags(args);
  if (!f.trust || !f.bundle) usage('--trust and --bundle are required (trust anchors are never fetched)');
  let trust, bundle;
  try {
    trust = await loadTrustStore(new Uint8Array(readFileSync(f.trust)));
    bundle = parseStrict(new Uint8Array(readFileSync(f.bundle)));
  } catch (e) {
    usage(`cannot load input: ${(e as Error).message}`);
  }
  let now: number | undefined;
  if (f.now !== undefined) {
    const t = parseTimestamp(f.now);
    if (t === null) usage('--now must be YYYY-MM-DDTHH:MM:SSZ');
    now = t;
  }
  const report = await verifyBundle(bundle, trust, now);
  process.stdout.write((f.format === 'json' ? JSON.stringify(report, null, 2) : text(report)) + '\n');
  const bad = failures(report, f['fail-on'] ? f['fail-on'].split(',') : []);
  if (bad.length > 0) {
    console.error(`steelmatter-verify: --fail-on dimensions not clean: ${bad.join(', ')}`);
    process.exit(1);
  }
}

async function cross(inPath: string, outPath: string): Promise<void> {
  const input = JSON.parse(readFileSync(inPath, 'utf8'));
  const trust = await loadTrustStore(input.trust_store);
  const { privateKey, publicKey } = await generateKeyPair('ES256', { extractable: true });
  const jwk = await exportJWK(publicKey);
  const kid = await calculateJwkThumbprint(jwk, 'sha256');
  const out: any = {
    trust_store: {
      trust_store_version: '1',
      keys: [{ kid, name: 'ts-fresh-signer', alg: 'ES256', synthetic: true, created_at: new Date().toISOString().slice(0, 19) + 'Z',
        purposes: ['SM-MANDATE+JWS', 'SM-PERMIT+JWS', 'SM-EXECUTION+JWS', 'SM-OBSERVATION+JWS'],
        jwk: { kty: 'EC', crv: 'P-256', x: jwk.x, y: jwk.y } }],
    },
    verified_go: [],
    signed: [],
  };
  for (const item of input.signed) {
    const v = await verify(item.jws, item.kind as SignedKind, trust);
    out.verified_go.push({ kind: item.kind, digest: v.digest });
    const jws = await sign(item.kind, v.payload, kid, privateKey);
    out.signed.push({ kind: item.kind, jws, signing_input: signingInput(item.kind, kid, v.payload) });
  }
  writeFileSync(outPath, JSON.stringify(out, null, 2) + '\n');
}

const [cmd, ...args] = process.argv.slice(2);
const run = cmd === 'verify' ? verifyCmd(args) : cmd === 'cross' && args.length === 2 ? cross(args[0], args[1]) : usage();
run.catch((e) => {
  console.error(`steelmatter-verify: ${e?.code ?? ''} ${e?.message ?? e}`);
  process.exit(2);
});
