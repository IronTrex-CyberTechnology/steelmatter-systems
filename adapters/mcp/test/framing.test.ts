import { test } from 'node:test';
import assert from 'node:assert/strict';
import { LineFramer } from '../src/framing.ts';

function capture(limit = 8) {
  const lines: string[] = [], errors: string[] = [];
  const framer = new LineFramer(line => lines.push(line), kind => errors.push(kind), limit);
  return {framer, lines, errors};
}

test('stdio framing accepts exact byte limit, CRLF, blanks and a final unterminated line', () => {
  const {framer, lines, errors} = capture();
  framer.push(Buffer.from('12345678\na\r\n\nend'));
  framer.end();
  assert.deepEqual(lines, ['12345678', 'a', '', 'end']);
  assert.deepEqual(errors, []);
});

test('stdio framing rejects overflow immediately, drains once and recovers at newline', () => {
  const {framer, lines, errors} = capture();
  for (const c of '123456789') framer.push(Buffer.from(c));
  assert.deepEqual(errors, ['size']);
  framer.push(Buffer.alloc(10000, 120));
  assert.deepEqual(errors, ['size']);
  framer.push(Buffer.from('\nok\n123456789\nlast'));
  framer.end();
  assert.deepEqual(lines, ['ok', 'last']);
  assert.deepEqual(errors, ['size', 'size']);
});

test('stdio framing counts bytes and preserves split UTF-8 without replacement decoding', () => {
  const {framer, lines, errors} = capture();
  for (const byte of Buffer.from('😀😀\n😀😀x\n')) framer.push(Buffer.from([byte]));
  framer.push(Buffer.from([0xff, 10, 0xc0, 0xaf, 10, 0xed, 0xa0, 0x80, 10]));
  framer.push(Buffer.from([0xef, 0xbb, 0xbf, 123, 125, 10]));
  framer.push(Buffer.from([0xf0, 0x9f]));
  framer.end();
  assert.deepEqual(lines, ['😀😀', '\ufeff{}']);
  assert.deepEqual(errors, ['size', 'encoding', 'encoding', 'encoding', 'encoding']);
});

test('stdio framing handles empty EOF and overflow EOF without extra messages', () => {
  const {framer, lines, errors} = capture(1);
  framer.end();
  framer.push(Buffer.from('xx'));
  framer.end();
  assert.deepEqual(lines, []);
  assert.deepEqual(errors, ['size']);
});
