// RFC 8785 canonical bytes and domain-separated digests, mirroring
// core/contract/canonical.go.
import { createHash } from 'node:crypto';
import canonicalizeLib from 'canonicalize';
import { parseStrict, type JsonValue } from './strict-json.ts';
import type { Kind } from './payloads.ts';

const canonicalize = canonicalizeLib as unknown as (v: unknown) => string | undefined;
const encoder = new TextEncoder();

/** RFC 8785 bytes of an already strictly parsed value. */
export function canonicalBytes(value: JsonValue): Uint8Array {
  const s = canonicalize(value);
  if (s === undefined) throw new Error('value cannot be canonicalised');
  return encoder.encode(s);
}

/** Strictly parses raw, then returns its RFC 8785 bytes. */
export function canonicalizeRaw(raw: Uint8Array): Uint8Array {
  return canonicalBytes(parseStrict(raw));
}

export const digestContext = (k: Kind) => `steelmatter:${k}:v1`;

/** Lowercase hex SHA-256 of ASCII(context) || 0x0A || canonical. */
export function digest(k: Kind, canonical: Uint8Array): string {
  return createHash('sha256').update(digestContext(k)).update('\n').update(canonical).digest('hex');
}
