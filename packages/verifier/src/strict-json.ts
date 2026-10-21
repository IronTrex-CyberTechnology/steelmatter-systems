// Strict JSON input profile, mirroring core/contract/strictjson.go:
// valid UTF-8 without BOM, no lone surrogates, no duplicate keys, integers
// only (no fraction, exponent or -0) within ±(2^53-1), depth ≤ 32, no
// trailing content. The first fault in document order decides the code.
import { ContractError } from './errors.ts';

export const MAX_PAYLOAD_BYTES = 1 << 20;
export const MAX_DEPTH = 32;
export const MAX_SAFE = Number.MAX_SAFE_INTEGER; // 2^53-1

export type JsonValue = null | boolean | number | string | JsonValue[] | { [k: string]: JsonValue };

const decoder = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });

export function parseStrict(input: Uint8Array): JsonValue {
  if (input.length > MAX_PAYLOAD_BYTES) {
    throw new ContractError('PAYLOAD_TOO_LARGE', '', `payload is ${input.length} bytes; limit ${MAX_PAYLOAD_BYTES}`);
  }
  let text: string;
  try {
    text = decoder.decode(input);
  } catch {
    throw new ContractError('INVALID_ENCODING', '', 'input is not valid UTF-8');
  }
  if (text.charCodeAt(0) === 0xfeff) {
    throw new ContractError('INVALID_ENCODING', '', 'byte-order mark is not allowed');
  }
  const p = new Parser(text);
  p.ws();
  const v = p.value(0, '');
  p.ws();
  if (p.pos !== text.length) throw p.syntax('', 'unexpected content after the top-level value');
  return v;
}

const escapePointer = (s: string) => s.replaceAll('~', '~0').replaceAll('/', '~1');
const isDigit = (c: string | undefined) => c !== undefined && c >= '0' && c <= '9';

class Parser {
  pos = 0;
  readonly s: string;
  constructor(s: string) {
    this.s = s;
  }

  syntax(path: string, msg: string): ContractError {
    return new ContractError('INVALID_JSON', path, `${msg} (offset ${this.pos})`);
  }

  ws(): void {
    while (this.pos < this.s.length) {
      const c = this.s[this.pos];
      if (c === ' ' || c === '\t' || c === '\n' || c === '\r') this.pos++;
      else return;
    }
  }

  value(depth: number, path: string): JsonValue {
    if (depth > MAX_DEPTH) throw this.syntax(path, 'nesting exceeds maximum depth');
    if (this.pos >= this.s.length) throw this.syntax(path, 'unexpected end of input');
    const c = this.s[this.pos];
    if (c === '{') return this.object(depth, path);
    if (c === '[') return this.array(depth, path);
    if (c === '"') return this.str(path);
    if (c === '-' || isDigit(c)) return this.number(path);
    for (const [word, val] of [['true', true], ['false', false], ['null', null]] as const) {
      if (this.s.startsWith(word, this.pos)) {
        this.pos += word.length;
        return val;
      }
    }
    throw this.syntax(path, 'unexpected character');
  }

  object(depth: number, path: string): JsonValue {
    this.pos++;
    const obj: { [k: string]: JsonValue } = Object.create(null);
    this.ws();
    if (this.s[this.pos] === '}') {
      this.pos++;
      return obj;
    }
    for (;;) {
      this.ws();
      if (this.s[this.pos] !== '"') throw this.syntax(path, 'expected object key');
      const key = this.str(path);
      const child = `${path}/${escapePointer(key)}`;
      if (Object.prototype.hasOwnProperty.call(obj, key)) {
        throw new ContractError('DUPLICATE_KEY', child, `duplicate object key ${JSON.stringify(key)}`);
      }
      this.ws();
      if (this.s[this.pos] !== ':') throw this.syntax(child, "expected ':'");
      this.pos++;
      this.ws();
      obj[key] = this.value(depth + 1, child);
      this.ws();
      if (this.pos >= this.s.length) throw this.syntax(path, 'unterminated object');
      const c = this.s[this.pos++];
      if (c === '}') return obj;
      if (c !== ',') {
        this.pos--;
        throw this.syntax(path, "expected ',' or '}'");
      }
    }
  }

  array(depth: number, path: string): JsonValue {
    this.pos++;
    const arr: JsonValue[] = [];
    this.ws();
    if (this.s[this.pos] === ']') {
      this.pos++;
      return arr;
    }
    for (let i = 0; ; i++) {
      this.ws();
      arr.push(this.value(depth + 1, `${path}/${i}`));
      this.ws();
      if (this.pos >= this.s.length) throw this.syntax(path, 'unterminated array');
      const c = this.s[this.pos++];
      if (c === ']') return arr;
      if (c !== ',') {
        this.pos--;
        throw this.syntax(path, "expected ',' or ']'");
      }
    }
  }

  str(path: string): string {
    this.pos++;
    let out = '';
    for (;;) {
      if (this.pos >= this.s.length) throw this.syntax(path, 'unterminated string');
      const c = this.s[this.pos];
      const code = c.charCodeAt(0);
      if (c === '"') {
        this.pos++;
        return out;
      }
      if (code < 0x20) throw this.syntax(path, 'unescaped control character in string');
      if (c !== '\\') {
        // Decoded from valid UTF-8, so surrogates here are always paired.
        out += c;
        this.pos++;
        continue;
      }
      this.pos++;
      const e = this.s[this.pos++];
      switch (e) {
        case '"': case '\\': case '/': out += e; break;
        case 'b': out += '\b'; break;
        case 'f': out += '\f'; break;
        case 'n': out += '\n'; break;
        case 'r': out += '\r'; break;
        case 't': out += '\t'; break;
        case 'u': {
          const hi = this.hex4(path);
          if (hi >= 0xd800 && hi <= 0xdbff) {
            if (this.s[this.pos] === '\\' && this.s[this.pos + 1] === 'u') {
              this.pos += 2;
              const lo = this.hex4(path);
              if (lo < 0xdc00 || lo > 0xdfff) throw new ContractError('INVALID_ENCODING', path, 'unpaired surrogate escape');
              out += String.fromCharCode(hi, lo);
            } else {
              throw new ContractError('INVALID_ENCODING', path, 'unpaired surrogate escape');
            }
          } else if (hi >= 0xdc00 && hi <= 0xdfff) {
            throw new ContractError('INVALID_ENCODING', path, 'unpaired surrogate escape');
          } else {
            out += String.fromCharCode(hi);
          }
          break;
        }
        case undefined: throw this.syntax(path, 'unterminated escape');
        default: throw this.syntax(path, 'invalid escape');
      }
    }
  }

  hex4(path: string): number {
    const h = this.s.slice(this.pos, this.pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(h)) throw this.syntax(path, 'invalid unicode escape');
    this.pos += 4;
    return parseInt(h, 16);
  }

  number(path: string): number {
    const start = this.pos;
    if (this.s[this.pos] === '-') this.pos++;
    const c = this.s[this.pos];
    if (c === '0') this.pos++;
    else if (c !== undefined && c >= '1' && c <= '9') while (isDigit(this.s[this.pos])) this.pos++;
    else throw this.syntax(path, 'invalid number');
    const intEnd = this.pos;
    let fractional = false;
    if (this.s[this.pos] === '.') {
      this.pos++;
      if (!isDigit(this.s[this.pos])) throw this.syntax(path, 'invalid fraction');
      while (isDigit(this.s[this.pos])) this.pos++;
      fractional = true;
    }
    if (this.s[this.pos] === 'e' || this.s[this.pos] === 'E') {
      this.pos++;
      if (this.s[this.pos] === '+' || this.s[this.pos] === '-') this.pos++;
      if (!isDigit(this.s[this.pos])) throw this.syntax(path, 'invalid exponent');
      while (isDigit(this.s[this.pos])) this.pos++;
      fractional = true;
    }
    const lit = this.s.slice(start, this.pos);
    if (fractional) throw new ContractError('NON_INTEGER_NUMBER', path, `number ${lit} is not an integer`);
    if (lit === '-0') throw new ContractError('NON_INTEGER_NUMBER', path, 'negative zero is not allowed');
    const n = BigInt(this.s.slice(start, intEnd));
    if (n > BigInt(MAX_SAFE) || n < -BigInt(MAX_SAFE)) {
      throw new ContractError('NUMBER_OUT_OF_RANGE', path, `integer ${lit} exceeds ±(2^53-1)`);
    }
    return Number(n);
  }
}
