// Browser stand-in for the one node:crypto call the verifier makes
// (canonical.ts: createHash('sha256').update(...).digest('hex')). Hashing is
// done by @noble/hashes, a maintained, audited library; nothing here is
// custom cryptography. Signature checks use jose with the browser's WebCrypto.
import { sha256 } from '@noble/hashes/sha2.js';
import { bytesToHex } from '@noble/hashes/utils.js';

const encoder = new TextEncoder();

export function createHash(algorithm: string) {
  if (algorithm !== 'sha256') throw new Error(`unsupported hash ${algorithm}`);
  const h = sha256.create();
  const hash = {
    update(data: string | Uint8Array) {
      h.update(typeof data === 'string' ? encoder.encode(data) : data);
      return hash;
    },
    digest(encoding: 'hex') {
      if (encoding !== 'hex') throw new Error(`unsupported digest encoding ${encoding}`);
      return bytesToHex(h.digest());
    },
  };
  return hash;
}
