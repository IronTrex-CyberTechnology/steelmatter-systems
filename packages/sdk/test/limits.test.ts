// Response size limit (16 MiB) against a real local HTTP server and the
// platform fetch. The limit must stop the download as soon as it is exceeded,
// not after buffering the whole body (memory exhaustion by a misbehaving or
// hostile server streaming without Content-Length).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer, type ServerResponse } from 'node:http';
import { SteelmatterClient, SteelmatterResponseError } from '../src/index.ts';

const MiB = 1 << 20;
const LIMIT = 16 * MiB;

interface Served { requests: number; sent: number; finished: boolean }

/** Serves `body(res, served)` and returns a client for it. */
async function serve(body: (res: ServerResponse, s: Served) => void) {
  const s: Served = { requests: 0, sent: 0, finished: false };
  const server = createServer((_req, res) => {
    s.requests++;
    body(res, s);
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const client = new SteelmatterClient({ baseUrl: `http://127.0.0.1:${(server.address() as { port: number }).port}`, token: 't' });
  return { client, s, close: () => new Promise<void>((r) => { server.closeAllConnections(); server.close(() => r()); }) };
}

/** Streams `total` bytes of JSON-ish padding in 1 MiB chunks, honouring backpressure, without Content-Length. */
function stream(total: number, headers: Record<string, string> = {}) {
  return (res: ServerResponse, s: Served) => {
    res.writeHead(200, { 'content-type': 'application/json', ...headers });
    const chunk = Buffer.alloc(MiB, 0x20);
    const pump = () => {
      while (s.sent < total) {
        if (res.destroyed) return;
        const n = Math.min(MiB, total - s.sent);
        s.sent += n;
        if (!res.write(n === MiB ? chunk : chunk.subarray(0, n))) { res.once('drain', pump); return; }
      }
      s.finished = true;
      res.end();
    };
    res.on('close', () => res.removeAllListeners('drain'));
    pump();
  };
}

test('chunked body without Content-Length: rejected soon after 16 MiB, not after downloading everything, and not retried', async () => {
  const { client, s, close } = await serve(stream(128 * MiB));
  try {
    const err = await client.getOperation('op').catch((e) => e);
    assert.ok(err instanceof SteelmatterResponseError, String(err));
    assert.match(err.message, /response larger than 16777216 bytes/);
    await new Promise((r) => setTimeout(r, 200)); // let the server notice the closed connection
    assert.equal(s.finished, false, 'the client must abandon the download');
    assert.ok(s.sent < 48 * MiB, `server pushed ${s.sent / MiB} MiB before the client stopped reading (limit 16 MiB plus socket buffers)`);
    assert.equal(s.requests, 1, 'an oversized response is not retried');
  } finally {
    await close();
  }
});

test('a declared Content-Length above the limit is rejected before reading the body', async () => {
  const { client, s, close } = await serve((res) => {
    res.writeHead(200, { 'content-type': 'application/json', 'content-length': String(LIMIT + 1) });
    res.write('{');
    // never sends the rest: the client must not wait for it
  });
  try {
    const err = await client.getOperation('op').catch((e) => e);
    assert.ok(err instanceof SteelmatterResponseError);
    assert.equal(s.requests, 1);
  } finally {
    await close();
  }
});

test('a body of exactly 16 MiB is accepted; one byte more is rejected', async () => {
  const json = (size: number) => {
    const head = '{"pad":"';
    return Buffer.from(head + 'x'.repeat(size - head.length - 2) + '"}');
  };
  for (const [size, ok] of [[LIMIT, true], [LIMIT + 1, false]] as const) {
    const payload = json(size);
    assert.equal(payload.length, size);
    const { client, close } = await serve((res) => {
      res.writeHead(200, { 'content-type': 'application/json' }); // chunked: no Content-Length
      for (let i = 0; i < payload.length; i += MiB) res.write(payload.subarray(i, i + MiB));
      res.end();
    });
    try {
      const r = await client.getOperation('op').then((x) => x, (e) => e);
      if (ok) assert.equal((r as { data: { pad: string } }).data.pad.length, size - 10, 'exactly the limit is accepted');
      else assert.ok(r instanceof SteelmatterResponseError, `${size} bytes must be rejected`);
    } finally {
      await close();
    }
  }
});

test('ordinary responses and empty bodies are unaffected', async () => {
  const { client, close } = await serve((res) => {
    res.writeHead(200, { 'content-type': 'application/json', 'x-request-id': 'req_' + 'e'.repeat(24) });
    res.end(JSON.stringify({ operation_id: 'op', execution: 'COMMITTED' }));
  });
  try {
    const r = await client.getOperation('op');
    assert.deepEqual(r.data, { operation_id: 'op', execution: 'COMMITTED' });
    assert.equal(r.requestId, 'req_' + 'e'.repeat(24));
  } finally {
    await close();
  }
});

test('invalid UTF-8 within the limit is still rejected as a contract violation', async () => {
  const { client, close } = await serve((res) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(Buffer.from([0x7b, 0x22, 0xff, 0x22, 0x7d]));
  });
  try {
    const err = await client.getOperation('op').catch((e) => e);
    assert.ok(err instanceof SteelmatterResponseError);
    assert.match(err.message, /not valid UTF-8/);
  } finally {
    await close();
  }
});
