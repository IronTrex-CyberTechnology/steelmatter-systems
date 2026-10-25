import { createHash } from 'node:crypto';
class ContractError extends TypeError {
    constructor(code, path, message){
        super(`${code} at ${path || '/'}: ${message}`);
        this.code = code;
    }
}
export const MAX_PAYLOAD_BYTES = 64 << 10;
export const MAX_DEPTH = 32;
export const MAX_SAFE = Number.MAX_SAFE_INTEGER;
const decoder = new TextDecoder('utf-8', {
    fatal: true,
    ignoreBOM: true
});
export function parseJSON(input) {
    if (!(input instanceof Uint8Array)) throw new TypeError('expected UTF-8 bytes');
    if (input.length > MAX_PAYLOAD_BYTES) {
        throw new ContractError('PAYLOAD_TOO_LARGE', '', `payload is ${input.length} bytes; limit ${MAX_PAYLOAD_BYTES}`);
    }
    let text;
    try {
        text = decoder.decode(input);
    } catch  {
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
const escapePointer = (s)=>s.replaceAll('~', '~0').replaceAll('/', '~1');
const isDigit = (c)=>c !== undefined && c >= '0' && c <= '9';
class Parser {
    pos = 0;
    s;
    constructor(s){
        this.s = s;
    }
    syntax(path, msg) {
        return new ContractError('INVALID_JSON', path, `${msg} (offset ${this.pos})`);
    }
    ws() {
        while(this.pos < this.s.length){
            const c = this.s[this.pos];
            if (c === ' ' || c === '\t' || c === '\n' || c === '\r') this.pos++;
            else return;
        }
    }
    value(depth, path) {
        if (depth > MAX_DEPTH) throw this.syntax(path, 'nesting exceeds maximum depth');
        if (this.pos >= this.s.length) throw this.syntax(path, 'unexpected end of input');
        const c = this.s[this.pos];
        if (c === '{') return this.object(depth, path);
        if (c === '[') return this.array(depth, path);
        if (c === '"') return this.str(path);
        if (c === '-' || isDigit(c)) return this.number(path);
        for (const [word, val] of [
            [
                'true',
                true
            ],
            [
                'false',
                false
            ],
            [
                'null',
                null
            ]
        ]){
            if (this.s.startsWith(word, this.pos)) {
                this.pos += word.length;
                return val;
            }
        }
        throw this.syntax(path, 'unexpected character');
    }
    object(depth, path) {
        this.pos++;
        const obj = Object.create(null);
        this.ws();
        if (this.s[this.pos] === '}') {
            this.pos++;
            return obj;
        }
        for(;;){
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
    array(depth, path) {
        this.pos++;
        const arr = [];
        this.ws();
        if (this.s[this.pos] === ']') {
            this.pos++;
            return arr;
        }
        for(let i = 0;; i++){
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
    str(path) {
        this.pos++;
        let out = '';
        for(;;){
            if (this.pos >= this.s.length) throw this.syntax(path, 'unterminated string');
            const c = this.s[this.pos];
            const code = c.charCodeAt(0);
            if (c === '"') {
                this.pos++;
                return out;
            }
            if (code < 0x20) throw this.syntax(path, 'unescaped control character in string');
            if (c !== '\\') {
                out += c;
                this.pos++;
                continue;
            }
            this.pos++;
            const e = this.s[this.pos++];
            switch(e){
                case '"':
                case '\\':
                case '/':
                    out += e;
                    break;
                case 'b':
                    out += '\b';
                    break;
                case 'f':
                    out += '\f';
                    break;
                case 'n':
                    out += '\n';
                    break;
                case 'r':
                    out += '\r';
                    break;
                case 't':
                    out += '\t';
                    break;
                case 'u':
                    {
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
                case undefined:
                    throw this.syntax(path, 'unterminated escape');
                default:
                    throw this.syntax(path, 'invalid escape');
            }
        }
    }
    hex4(path) {
        const h = this.s.slice(this.pos, this.pos + 4);
        if (!/^[0-9a-fA-F]{4}$/.test(h)) throw this.syntax(path, 'invalid unicode escape');
        this.pos += 4;
        return parseInt(h, 16);
    }
    number(path) {
        const start = this.pos;
        if (this.s[this.pos] === '-') this.pos++;
        const c = this.s[this.pos];
        if (c === '0') this.pos++;
        else if (c !== undefined && c >= '1' && c <= '9') while(isDigit(this.s[this.pos]))this.pos++;
        else throw this.syntax(path, 'invalid number');
        const intEnd = this.pos;
        let fractional = false;
        if (this.s[this.pos] === '.') {
            this.pos++;
            if (!isDigit(this.s[this.pos])) throw this.syntax(path, 'invalid fraction');
            while(isDigit(this.s[this.pos]))this.pos++;
            fractional = true;
        }
        if (this.s[this.pos] === 'e' || this.s[this.pos] === 'E') {
            this.pos++;
            if (this.s[this.pos] === '+' || this.s[this.pos] === '-') this.pos++;
            if (!isDigit(this.s[this.pos])) throw this.syntax(path, 'invalid exponent');
            while(isDigit(this.s[this.pos]))this.pos++;
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
export function canonical(value) {
    const parts = [];
    const active = new Set();
    let size = 0;
    function emit(text) {
        size += Buffer.byteLength(text);
        if (size > MAX_PAYLOAD_BYTES) throw new TypeError('payload too large');
        parts.push(text);
    }
    function walk(v, depth) {
        if (depth > MAX_DEPTH) throw new TypeError('maximum depth exceeded');
        if (v === null || typeof v === 'boolean') {
            emit(String(v));
            return;
        }
        if (typeof v === 'number') {
            if (!Number.isSafeInteger(v) || Object.is(v, -0)) throw new TypeError('expected safe integer');
            emit(String(v));
            return;
        }
        if (typeof v === 'string') {
            if (!v.isWellFormed() || v.length > MAX_PAYLOAD_BYTES) throw new TypeError('invalid string');
            emit(JSON.stringify(v));
            return;
        }
        if (!v || typeof v !== 'object' || active.has(v)) throw new TypeError('expected acyclic JSON data');
        const proto = Object.getPrototypeOf(v);
        const arr = Array.isArray(v);
        if (arr ? proto !== Array.prototype : proto !== null && proto !== Object.prototype) throw new TypeError('custom prototype not allowed');
        const descriptors = Object.getOwnPropertyDescriptors(v);
        if (Reflect.ownKeys(v).some((k)=>typeof k !== 'string')) throw new TypeError('symbol keys not allowed');
        const keys = Object.keys(descriptors).filter((k)=>!(arr && k === 'length'));
        for (const key of keys){
            if (!('value' in descriptors[key]) || !descriptors[key].enumerable || !key.isWellFormed()) throw new TypeError('only enumerable data properties allowed');
        }
        active.add(v);
        if (arr) {
            if (v.length > 4096 || keys.length !== v.length || keys.some((k, i)=>k !== String(i))) throw new TypeError('sparse or extended array not allowed');
            emit('[');
            for(let i = 0; i < v.length; i++){
                if (i) emit(',');
                walk(descriptors[i].value, depth + 1);
            }
            emit(']');
        } else {
            if (keys.length > 512) throw new TypeError('too many object keys');
            emit('{');
            for (const [i, key] of keys.sort().entries()){
                if (i) emit(',');
                emit(JSON.stringify(key));
                emit(':');
                walk(descriptors[key].value, depth + 1);
            }
            emit('}');
        }
        active.delete(v);
    }
    walk(value, 0);
    return parts.join('');
}
export function snapshot(value) {
    const clone = parseJSON(Buffer.from(canonical(value), 'utf8'));
    function freeze(v) {
        if (v && typeof v === 'object') {
            Object.values(v).forEach(freeze);
            Object.freeze(v);
        }
        return v;
    }
    return freeze(clone);
}
export function digest(kind, value) {
    return 'sha256:' + createHash('sha256').update('steelmatter-destination-kit/0.1/' + kind + '\0').update(canonical(value)).digest('hex');
}
