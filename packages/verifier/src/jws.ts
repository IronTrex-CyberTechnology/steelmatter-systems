// SM-JWS-ES256-1 compact JWS, mirroring core/contract/jws.go including the
// check order. Verification establishes integrity and signer attribution only.
import { CompactSign, compactVerify, type CryptoKey } from 'jose';
import { ContractError } from './errors.ts';
import { parseStrict } from './strict-json.ts';
import { validate, type Kind } from './payloads.ts';
import { canonicalBytes, digest } from './canonical.ts';
import { MAX_PAYLOAD_BYTES } from './strict-json.ts';
import type { TrustStore, TrustedKey } from './trust.ts';

export type SignedKind = 'mandate' | 'permit' | 'execution' | 'observation';
export const JWS_TYPES: Record<SignedKind, string> = {
  mandate: 'SM-MANDATE+JWS',
  permit: 'SM-PERMIT+JWS',
  execution: 'SM-EXECUTION+JWS',
  observation: 'SM-OBSERVATION+JWS',
};
export const MAX_JWS_BYTES = Math.floor((MAX_PAYLOAD_BYTES * 4) / 3) + 4096;

const B64URL = /^[A-Za-z0-9_-]*$/;
const encoder = new TextEncoder();

function b64decode(s: string): Uint8Array | null {
  if (!B64URL.test(s) || s.length % 4 === 1) return null;
  const bytes = Buffer.from(s, 'base64url');
  // Reject non-zero trailing bits: re-encoding must reproduce the input.
  return bytes.toString('base64url') === s ? new Uint8Array(bytes) : null;
}
const b64encode = (b: Uint8Array) => Buffer.from(b).toString('base64url');

/** The pinned protected header: exactly alg, kid, typ in that order, no whitespace. */
export function protectedHeader(kind: SignedKind, kid: string): string {
  if (!kid || !B64URL.test(kid)) throw new Error('kid must be a base64url thumbprint');
  return `{"alg":"ES256","kid":"${kid}","typ":"${JWS_TYPES[kind]}"}`;
}

export function signingInput(kind: SignedKind, kid: string, canonicalPayload: Uint8Array): string {
  return `${b64encode(encoder.encode(protectedHeader(kind, kid)))}.${b64encode(canonicalPayload)}`;
}

/** Validates raw as kind, canonicalises it and signs it. */
export async function sign(kind: SignedKind, raw: Uint8Array, kid: string, key: CryptoKey): Promise<string> {
  const canonical = canonicalBytes(validate(kind, raw));
  const jws = await new CompactSign(canonical)
    .setProtectedHeader({ alg: 'ES256', kid, typ: JWS_TYPES[kind] })
    .sign(key);
  if (!jws.startsWith(signingInput(kind, kid, canonical) + '.')) {
    throw new Error('JWS library produced an unpinned signing input');
  }
  return jws;
}

export interface Verified {
  kind: SignedKind;
  kid: string;
  payload: Uint8Array;
  digest: string;
  value: Record<string, unknown>;
  key: TrustedKey;
}

export async function verify(compact: string, kind: SignedKind, trust: TrustStore): Promise<Verified> {
  const typ = JWS_TYPES[kind];
  if (!typ) throw new ContractError('JWS_TYPE_MISMATCH', '', `kind ${kind} is not signed`);
  if (compact.length > MAX_JWS_BYTES) throw new ContractError('JWS_MALFORMED', '', 'JWS too large');
  const parts = compact.split('.');
  if (parts.length !== 3) throw new ContractError('JWS_MALFORMED', '', 'compact JWS must have three segments');
  const dec = parts.map(b64decode);
  dec.forEach((d, i) => {
    if (d === null) throw new ContractError('JWS_MALFORMED', '', `segment ${i} is not unpadded base64url`);
  });
  const [headerBytes, , sig] = dec as Uint8Array[];
  let header: any;
  try {
    header = parseStrict(headerBytes);
  } catch (e) {
    throw new ContractError('JWS_MALFORMED', '', `protected header: ${(e as Error).message}`);
  }
  if (typeof header !== 'object' || header === null || Array.isArray(header)) {
    throw new ContractError('JWS_MALFORMED', '', 'protected header is not an object');
  }
  if (header.alg !== 'ES256') throw new ContractError('JWS_ALG_UNSUPPORTED', '', 'alg must be ES256');
  for (const name of Object.keys(header).sort()) {
    if (name !== 'alg' && name !== 'kid' && name !== 'typ') {
      throw new ContractError('JWS_HEADER_UNSUPPORTED', '', `header member "${name}" is not supported`);
    }
  }
  if (header.typ !== typ) throw new ContractError('JWS_TYPE_MISMATCH', '', `typ must be ${typ}`);
  const kid = typeof header.kid === 'string' ? header.kid : '';
  const tk = trust.lookup(kid);
  if (!tk || !tk.purposes.includes(typ)) throw new ContractError('JWS_KEY_UNTRUSTED', '', `kid is not trusted for ${typ}`);
  if (Buffer.from(headerBytes).toString('utf8') !== protectedHeader(kind, kid)) {
    throw new ContractError('JWS_MALFORMED', '', 'protected header is not in pinned form');
  }
  if (sig.length !== 64) throw new ContractError('JWS_SIGNATURE_INVALID', '', 'ES256 signature must be 64 bytes');
  let payload: Uint8Array;
  try {
    payload = (await compactVerify(compact, await trust.publicKey(kid), { algorithms: ['ES256'] })).payload;
  } catch {
    throw new ContractError('JWS_SIGNATURE_INVALID', '', 'signature does not verify');
  }
  const value = validate(kind, payload);
  const canonical = canonicalBytes(value);
  if (Buffer.compare(Buffer.from(canonical), Buffer.from(payload)) !== 0) {
    throw new ContractError('JWS_PAYLOAD_NOT_CANONICAL', '', 'payload is not RFC 8785 canonical');
  }
  return { kind, kid, payload, digest: digest(kind, payload), value, key: tk };
}
