// Minimal browser stand-in for the Buffer calls in the verifier's jws.ts:
// Buffer.from(base64url string | utf8 string | bytes), toString('base64url' |
// 'utf8') and Buffer.compare. Encoding only; no cryptography. jws.ts checks
// the base64url alphabet and round-trips every decode itself, and the demo
// tests run every golden vector through this bundle and the Node verifier and
// require identical results.
const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';
const LOOKUP = new Map([...B64].map((c, i) => [c, i]));
const encoder = new TextEncoder();
const decoder = new TextDecoder('utf-8');

function decodeBase64url(s: string): Uint8Array {
  const out: number[] = [];
  let acc = 0;
  let bits = 0;
  for (const c of s) {
    const v = LOOKUP.get(c);
    if (v === undefined) break; // Node stops at the first invalid character
    acc = (acc << 6) | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push((acc >> bits) & 0xff);
    }
  }
  return new Uint8Array(out);
}

function encodeBase64url(b: Uint8Array): string {
  let s = '';
  let i = 0;
  for (; i + 2 < b.length; i += 3) {
    const n = (b[i] << 16) | (b[i + 1] << 8) | b[i + 2];
    s += B64[(n >> 18) & 63] + B64[(n >> 12) & 63] + B64[(n >> 6) & 63] + B64[n & 63];
  }
  if (i < b.length) {
    const n = (b[i] << 16) | ((b[i + 1] ?? 0) << 8);
    s += B64[(n >> 18) & 63] + B64[(n >> 12) & 63];
    if (i + 1 < b.length) s += B64[(n >> 6) & 63];
  }
  return s;
}

class BufferBytes extends Uint8Array {
  override toString(encoding?: string): string {
    if (encoding === 'base64url') return encodeBase64url(this);
    if (encoding === undefined || encoding === 'utf8' || encoding === 'utf-8') return decoder.decode(this);
    throw new Error(`unsupported encoding ${encoding}`);
  }
}

export const Buffer = {
  from(value: string | ArrayLike<number>, encoding?: string): BufferBytes {
    if (typeof value === 'string') {
      if (encoding === 'base64url') return new BufferBytes(decodeBase64url(value));
      if (encoding === undefined || encoding === 'utf8' || encoding === 'utf-8') return new BufferBytes(encoder.encode(value));
      throw new Error(`unsupported encoding ${encoding}`);
    }
    const b = new BufferBytes(value.length);
    b.set(value);
    return b;
  },

  compare(a: Uint8Array, b: Uint8Array): number {
    const n = Math.min(a.length, b.length);
    for (let i = 0; i < n; i++) if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
    return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
  },
};
