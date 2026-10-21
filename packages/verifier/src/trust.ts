// Explicit trust anchors: a local trust store document (core/keys). There is
// no remote key fetch; keys come only from the caller.
import { calculateJwkThumbprint, importJWK, type CryptoKey } from 'jose';
import { parseStrict, type JsonValue } from './strict-json.ts';

export interface PublicJWK { kty: 'EC'; crv: 'P-256'; x: string; y: string }
export interface TrustedKey {
  kid: string;
  name: string;
  alg: 'ES256';
  synthetic: boolean;
  purposes: string[];
  created_at: string;
  /** Retired from this instant: still verifies records signed before it. */
  not_after?: string;
  jwk: PublicJWK;
}
export interface TrustStore {
  readonly keys: readonly TrustedKey[];
  lookup(kid: string): TrustedKey | undefined;
  publicKey(kid: string): Promise<CryptoKey>;
}

/** The only valid key purposes: the four signed JWS types (I02 §5; CLR-03). */
export const KNOWN_PURPOSES: readonly string[] = ['SM-MANDATE+JWS', 'SM-PERMIT+JWS', 'SM-EXECUTION+JWS', 'SM-OBSERVATION+JWS'];

const STORE_FIELDS = ['keys', 'trust_store_version'];
const KEY_FIELDS = ['alg', 'created_at', 'jwk', 'kid', 'name', 'purposes', 'synthetic'];
const JWK_FIELDS = ['crv', 'kty', 'x', 'y'];

function closed(o: any, fields: string[], what: string, optional: string[] = []): void {
  if (typeof o !== 'object' || o === null || Array.isArray(o)) throw new Error(`${what} must be an object`);
  for (const k of Object.keys(o)) if (!fields.includes(k) && !optional.includes(k)) throw new Error(`${what} has unknown field ${k}`);
  for (const k of fields) if (!(k in o)) throw new Error(`${what} lacks ${k}`);
}

// RFC 3339 date-time with real calendar values (matches Go's time.Time
// decoding of created_at, so both runtimes accept and reject the same values).
const RFC3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d+)?(Z|[+-](\d{2}):(\d{2}))$/;
function validRFC3339(s: unknown): boolean {
  if (typeof s !== 'string') return false;
  const m = RFC3339.exec(s);
  if (!m) return false;
  const [y, mo, d, h, mi, se] = m.slice(1, 7).map(Number);
  const leap = (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0;
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][mo - 1];
  if (mo < 1 || mo > 12 || d < 1 || d > days || h > 23 || mi > 59 || se > 59) return false;
  if (m[9] !== undefined && (Number(m[9]) > 23 || Number(m[10]) > 59)) return false;
  return true;
}

/**
 * Parses and checks a trust store: closed fields, field types, ES256 P-256,
 * kid = RFC 7638 thumbprint, at least one purpose, and no repeated kid or
 * name (QA-D1-002). Same accept/reject rules as Go keys.ParseTrustStore,
 * pinned by fixtures/vectors/truststore-v1.json.
 */
export async function loadTrustStore(doc: Uint8Array | JsonValue): Promise<TrustStore> {
  const o: any = doc instanceof Uint8Array ? parseStrict(doc) : doc;
  closed(o, STORE_FIELDS, 'trust store');
  if (o.trust_store_version !== '1') throw new Error('unsupported trust store version');
  if (!Array.isArray(o.keys)) throw new Error('trust store keys must be an array');
  const keys: TrustedKey[] = [];
  const cache = new Map<string, CryptoKey>();
  const kids = new Set<string>();
  const names = new Set<string>();
  for (const k of o.keys as any[]) {
    closed(k, KEY_FIELDS, 'trust store key', ['not_after']);
    if ('not_after' in k) {
      if (!validRFC3339(k.not_after)) throw new Error(`key ${k.kid}: not_after must be a valid RFC 3339 date-time`);
      if (!(Date.parse(k.not_after) > Date.parse(k.created_at))) throw new Error(`key ${k.kid}: not_after must be after created_at`);
    }
    closed(k.jwk, JWK_FIELDS, 'trust store jwk');
    if (typeof k.kid !== 'string' || typeof k.name !== 'string' || typeof k.synthetic !== 'boolean' || !validRFC3339(k.created_at)) {
      throw new Error('trust store key has a field of the wrong type or an invalid created_at');
    }
    if (typeof k.jwk.x !== 'string' || typeof k.jwk.y !== 'string') throw new Error('trust store jwk coordinates must be strings');
    // A repeated kid could carry different purposes, and lookup order would
    // then decide what a key may sign; a repeated name is ambiguous.
    if (kids.has(k.kid) || names.has(k.name)) throw new Error(`trust store repeats kid ${k.kid} or name ${k.name}`);
    kids.add(k.kid);
    names.add(k.name);
    if (k.alg !== 'ES256' || k.jwk.kty !== 'EC' || k.jwk.crv !== 'P-256') throw new Error(`key ${k.kid}: only ES256 P-256`);
    // Only the four exact JWS types; unknown values, other letter case,
    // whitespace and repeats are rejected at load (CLR-03).
    if (!Array.isArray(k.purposes) || k.purposes.length === 0) throw new Error(`key ${k.kid}: at least one purpose is required`);
    const seenPurposes = new Set<string>();
    for (const p of k.purposes) {
      if (typeof p !== 'string' || !KNOWN_PURPOSES.includes(p)) throw new Error(`key ${k.kid}: unknown purpose ${JSON.stringify(p)}`);
      if (seenPurposes.has(p)) throw new Error(`key ${k.kid}: purpose ${p} is repeated`);
      seenPurposes.add(p);
    }
    const thumb = await calculateJwkThumbprint({ kty: 'EC', crv: 'P-256', x: k.jwk.x, y: k.jwk.y }, 'sha256');
    if (thumb !== k.kid) throw new Error(`key ${k.kid}: kid is not the RFC 7638 thumbprint`);
    // Importing validates the point.
    cache.set(k.kid, (await importJWK({ kty: 'EC', crv: 'P-256', x: k.jwk.x, y: k.jwk.y }, 'ES256')) as CryptoKey);
    keys.push(k);
  }
  return {
    keys,
    lookup: (kid) => keys.find((k) => k.kid === kid),
    publicKey: async (kid) => {
      const key = cache.get(kid);
      if (!key) throw new Error('unknown kid');
      return key;
    },
  };
}
