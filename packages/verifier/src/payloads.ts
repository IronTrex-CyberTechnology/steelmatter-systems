// Payload validation mirroring core/contract/payloads.go. Check order:
// strict parse; top-level object; schema_version; crypto_profile; unknown
// fields; currency; timestamps; JSON Schema; semantic rules.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { ContractError } from './errors.ts';
import { parseStrict, MAX_SAFE, type JsonValue } from './strict-json.ts';

const require = createRequire(import.meta.url);
const Ajv2020 = require('ajv/dist/2020.js').default;
const addFormats = require('ajv-formats').default;

export type Kind = 'mandate' | 'action' | 'permit' | 'execution' | 'observation';
export const KINDS: readonly Kind[] = ['mandate', 'action', 'permit', 'execution', 'observation'];
export const SCHEMA_VERSION = '1';
export const CRYPTO_PROFILE = 'SM-JWS-ES256-1';
export const CURRENCY = 'INR';
export const MAX_PERMIT_LIFETIME_SECONDS = 300;

const TIMESTAMP_FIELDS: Record<Kind, string[]> = {
  mandate: ['issued_at', 'expires_at'],
  action: [],
  permit: ['issued_at', 'expires_at'],
  execution: ['committed_at', 'admitted_at'],
  observation: ['observed_at'],
};

type Schema = { properties: Record<string, any> };
const ajv = new Ajv2020({ allErrors: false, strict: true });
addFormats(ajv);
const schemas = {} as Record<Kind, Schema>;
const validators = {} as Record<Kind, (v: unknown) => boolean>;
for (const k of KINDS) {
  const s = JSON.parse(readFileSync(new URL(`../schemas/${k}.schema.json`, import.meta.url), 'utf8'));
  schemas[k] = s;
  validators[k] = ajv.compile(s);
}
const lineFields = new Set(Object.keys(schemas.action.properties.lines.items.properties));

const isObject = (v: JsonValue): v is { [k: string]: JsonValue } =>
  typeof v === 'object' && v !== null && !Array.isArray(v);
const sortedKeys = (o: object) => Object.keys(o).sort(byteCompare);
// Identifiers are restricted ASCII, so UTF-16 order equals byte order; this
// comparator is still explicit about it.
export function byteCompare(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** Parses and validates bytes as kind; returns the payload object. */
export function validate(kind: Kind, raw: Uint8Array): { [k: string]: JsonValue } {
  const tree = parseStrict(raw);
  if (!isObject(tree)) throw new ContractError('SCHEMA_VIOLATION', '', 'payload must be a JSON object');
  if (tree.schema_version !== SCHEMA_VERSION && !(tree.schema_version === '2' && (kind === 'execution' || kind === 'observation'))) {
    throw new ContractError('UNSUPPORTED_VERSION', '/schema_version', `schema_version must be "${SCHEMA_VERSION}"`);
  }
  if (tree.crypto_profile !== CRYPTO_PROFILE) {
    throw new ContractError('UNSUPPORTED_PROFILE', '/crypto_profile', `crypto_profile must be "${CRYPTO_PROFILE}"`);
  }
  const fields = schemas[kind]?.properties;
  if (!fields) throw new ContractError('SCHEMA_VIOLATION', '', `unknown payload kind ${kind}`);
  for (const f of sortedKeys(tree)) {
    if (!Object.prototype.hasOwnProperty.call(fields, f)) {
      throw new ContractError('UNKNOWN_FIELD', `/${f}`, `field "${f}" is not part of the ${kind} payload`);
    }
  }
  if (kind === 'action' && Array.isArray(tree.lines)) {
    tree.lines.forEach((l, i) => {
      if (isObject(l)) {
        for (const f of sortedKeys(l)) {
          if (!lineFields.has(f)) throw new ContractError('UNKNOWN_FIELD', `/lines/${i}/${f}`, `field "${f}" is not part of a line`);
        }
      }
    });
  }
  if ('currency' in tree && tree.currency !== CURRENCY) {
    throw new ContractError('UNSUPPORTED_CURRENCY', '/currency', `only ${CURRENCY} is supported`);
  }
  for (const f of TIMESTAMP_FIELDS[kind]) {
    const v = tree[f];
    if (typeof v === 'string' && parseTimestamp(v) === null) {
      throw new ContractError('INVALID_TIMESTAMP', `/${f}`, `timestamp "${v}" is not a real UTC time YYYY-MM-DDTHH:MM:SSZ`);
    }
  }
  const validator = validators[kind] as ((v: unknown) => boolean) & { errors?: { instancePath: string; message?: string }[] };
  if (!validator(tree)) {
    const e = validator.errors?.[0];
    throw new ContractError('SCHEMA_VIOLATION', e?.instancePath ?? '', e?.message ?? 'schema violation');
  }
  checkSemantics(kind, tree);
  return tree;
}

const TS_RE = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})Z$/;

/** Returns epoch seconds for a real UTC whole-second timestamp, else null. Leap seconds are rejected. */
export function parseTimestamp(s: string): number | null {
  const m = TS_RE.exec(s);
  if (!m) return null;
  const [y, mo, d, h, mi, se] = m.slice(1).map(Number);
  if (mo < 1 || mo > 12 || h > 23 || mi > 59 || se > 59 || d < 1) return null;
  const leap = (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][mo - 1];
  if (d > days) return null;
  const date = new Date(0);
  date.setUTCFullYear(y, mo - 1, d);
  date.setUTCHours(h, mi, se, 0);
  return date.getTime() / 1000;
}

function checkSemantics(kind: Kind, p: { [k: string]: any }): void {
  if (kind === 'action') checkAction(p);
  if (kind === 'mandate') checkRange(p.issued_at, p.expires_at, 0);
  if (kind === 'permit') checkRange(p.issued_at, p.expires_at, MAX_PERMIT_LIFETIME_SECONDS);
  if (kind === 'execution' && p.schema_version === '2' && parseTimestamp(p.admitted_at)! > parseTimestamp(p.committed_at)!) throw new ContractError('INVALID_TIME_RANGE', '/committed_at', 'settlement precedes admission');
  if (kind === 'observation') checkObservation(p as Parameters<typeof checkObservation>[0]);
}

// D1 v1.4 tax rules (mirrors checkAction in core/contract/payloads.go):
// tax_ids sorted and unique per line; amount_untaxed_minor and tax_minor
// together, untaxed = line total, amount = untaxed + tax; no tax_ids without them.
function checkAction(p: { [k: string]: any }): void {
  const lines = p.lines as { item_id: string; quantity: number; unit_price_minor: number; tax_ids?: string[] }[];
  let taxed = false;
  lines.forEach((l, i) => {
    const t = l.tax_ids ?? [];
    for (let j = 1; j < t.length; j++) {
      if (byteCompare(t[j - 1], t[j]) >= 0) throw new ContractError('LINES_NOT_CANONICAL', `/lines/${i}/tax_ids/${j}`, 'tax_ids must be sorted and unique');
    }
    taxed = taxed || t.length > 0;
  });
  const hasUntaxed = 'amount_untaxed_minor' in p;
  if (hasUntaxed !== ('tax_minor' in p)) {
    throw new ContractError('SCHEMA_VIOLATION', '/tax_minor', 'amount_untaxed_minor and tax_minor must be given together');
  }
  if (!hasUntaxed) {
    if (taxed) throw new ContractError('SCHEMA_VIOLATION', '/tax_minor', 'lines with tax_ids require amount_untaxed_minor and tax_minor');
    checkLines(lines, p.amount_minor);
    return;
  }
  try {
    checkLines(lines, p.amount_untaxed_minor);
  } catch (e) {
    if (e instanceof ContractError && e.code === 'AMOUNT_MISMATCH') {
      throw new ContractError('AMOUNT_MISMATCH', '/amount_untaxed_minor', e.message.slice(e.message.indexOf(': ') + 2));
    }
    throw e;
  }
  const sum = BigInt(p.amount_untaxed_minor) + BigInt(p.tax_minor);
  if (sum > BigInt(MAX_SAFE)) throw new ContractError('ARITHMETIC_OVERFLOW', '/amount_minor', 'amount_untaxed_minor + tax_minor exceeds 2^53-1');
  if (sum !== BigInt(p.amount_minor)) {
    throw new ContractError('AMOUNT_MISMATCH', '/amount_minor', `amount_minor ${p.amount_minor} does not equal amount_untaxed_minor ${p.amount_untaxed_minor} + tax_minor ${p.tax_minor}`);
  }
}

function checkLines(lines: { item_id: string; quantity: number; unit_price_minor: number }[], amount: number): void {
  const seen = new Set<string>();
  lines.forEach((l, i) => {
    if (seen.has(l.item_id)) throw new ContractError('DUPLICATE_LINE_ITEM', `/lines/${i}/item_id`, `item_id "${l.item_id}" appears more than once`);
    seen.add(l.item_id);
  });
  for (let i = 1; i < lines.length; i++) {
    if (byteCompare(lines[i - 1].item_id, lines[i].item_id) >= 0) {
      throw new ContractError('LINES_NOT_CANONICAL', `/lines/${i}`, 'lines must be sorted by item_id');
    }
  }
  const max = BigInt(MAX_SAFE);
  let total = 0n;
  lines.forEach((l, i) => {
    const product = BigInt(l.quantity) * BigInt(l.unit_price_minor);
    if (product > max) throw new ContractError('ARITHMETIC_OVERFLOW', `/lines/${i}`, 'quantity × unit_price_minor exceeds 2^53-1');
    if (total + product > max) throw new ContractError('ARITHMETIC_OVERFLOW', '/lines', 'line total exceeds 2^53-1');
    total += product;
  });
  if (total !== BigInt(amount)) {
    throw new ContractError('AMOUNT_MISMATCH', '/amount_minor', `amount_minor ${amount} does not equal line total ${total}`);
  }
}

function checkRange(issued: string, expires: string, maxSeconds: number): void {
  const i = parseTimestamp(issued)!;
  const e = parseTimestamp(expires)!;
  if (!(e > i)) throw new ContractError('INVALID_TIME_RANGE', '/expires_at', 'expires_at must be after issued_at');
  if (maxSeconds > 0 && e - i > maxSeconds) {
    throw new ContractError('INVALID_TIME_RANGE', '/expires_at', `lifetime ${e - i}s exceeds ${maxSeconds}s`);
  }
}

function checkObservation(o: { [k: string]: any }): void {
  const obs = o.observed_action_digest;
  let matches = obs !== null && obs === o.expected_action_digest;
  if (o.schema_version === '2') matches = matches && o.observed_company_ref === o.expected_company_ref &&
    o.observed_state === 'purchase' && o.observed_permit_digest === o.expected_permit_digest;
  const bad = (o.status === 'MATCH' && !matches) ||
    (o.status === 'MISMATCH' && (obs === null || matches)) ||
    (o.status === 'INSUFFICIENT_EVIDENCE' && obs !== null);
  if (bad) throw new ContractError('OBSERVATION_INCONSISTENT', '/status', `status ${o.status} is inconsistent with destination evidence`);
}
