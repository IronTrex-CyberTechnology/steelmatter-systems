// Transport security (QA SC2 / SDK-F01, SC3 / SDK-F06), against real local
// HTTP servers and the platform fetch.
// SC2: a redirect is a response outside the contract. It is refused with
//      SteelmatterResponseError, never followed and never retried, so the
//      bearer token reaches only the configured origin.
// SC3: bearer tokens never travel over plain HTTP to another machine; http is
//      accepted only for loopback hosts.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer, type IncomingMessage } from 'node:http';
import { SteelmatterClient, SteelmatterResponseError, SteelmatterTransportError } from '../src/index.ts';
import { isLoopback } from '../src/client.ts';

async function listen(handler: (req: IncomingMessage, res: import('node:http').ServerResponse) => void) {
  const seen: { method: string; url: string; auth: string | undefined }[] = [];
  const server = createServer((req, res) => {
    seen.push({ method: req.method ?? '', url: req.url ?? '', auth: req.headers.authorization });
    req.resume();
    handler(req, res);
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const url = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  return { url, seen, close: () => new Promise<void>((r) => { server.closeAllConnections(); server.close(() => r()); }) };
}

const mandate = {
  actor_ids: ['agent-a'], audience: 'synthetic-procurement', supplier_ids: ['supplier-a'], delivery_ids: ['warehouse-a'],
  currency: 'INR' as const, limit_minor: 100_000_000, expires_at: '2030-01-01T00:00:00Z',
};

for (const status of [301, 302, 303, 307, 308]) {
  test(`SC2: HTTP ${status} redirect is refused as SteelmatterResponseError, never followed and never retried`, async () => {
    const target = await listen((_q, res) => { res.writeHead(200, { 'content-type': 'application/json' }); res.end('{}'); });
    const origin = await listen((_q, res) => { res.writeHead(status, { location: `${target.url}/stolen` }); res.end(); });
    try {
      const c = new SteelmatterClient({ baseUrl: origin.url, token: 'tok-secret', retry: { maxAttempts: 3, baseDelayMs: 1, maxDelayMs: 1 } });
      for (const call of [() => c.getOperation('op-1'), () => c.createMandate(mandate as never)]) {
        const before = origin.seen.length;
        const err = await call().then(() => assert.fail('a redirect must not succeed'), (e: unknown) => e);
        assert.ok(err instanceof SteelmatterResponseError, `expected SteelmatterResponseError, got ${(err as Error)?.constructor?.name}`);
        assert.ok(!(err instanceof SteelmatterTransportError));
        assert.equal(err.status, status);
        assert.match(err.message, /refused redirect/);
        assert.equal(origin.seen.length - before, 1, 'exactly one attempt: a redirect is not retried');
      }
      assert.equal(target.seen.length, 0, 'the redirect target received no request, so the token never left the origin');
    } finally {
      await origin.close();
      await target.close();
    }
  });
}

test('SC3: a non-loopback http base URL is refused at construction, before any request', () => {
  for (const baseUrl of ['http://steelmatter.invalid', 'http://example.com:8080', 'http://10.0.0.5', 'http://0.0.0.0:8080', 'http://localhost.example.com', 'http://[::2]', 'http://127.example.com']) {
    assert.throws(() => new SteelmatterClient({ baseUrl, token: 't', fetch: (() => assert.fail('no request may be made')) as never }), /must use https unless the host is loopback/, baseUrl);
  }
});

test('SC3: https is accepted for any host, http only for loopback', () => {
  for (const baseUrl of ['https://steelmatter.invalid', 'https://example.com:8443/api', 'http://127.0.0.1:8080', 'http://127.1.2.3', 'http://localhost:8080', 'http://[::1]:8080']) {
    assert.doesNotThrow(() => new SteelmatterClient({ baseUrl, token: 't' }), baseUrl);
  }
  for (const h of ['localhost', '127.0.0.1', '127.255.255.255', '[::1]']) assert.equal(isLoopback(h), true, h);
  for (const h of ['localhost.example.com', '128.0.0.1', '0.0.0.0', '[::2]', 'steelmatter.invalid']) assert.equal(isLoopback(h), false, h);
});

test('SC3: other unsafe base URLs are still refused', () => {
  for (const baseUrl of ['ftp://x', 'https://u:p@x', 'https://x/?q=1', 'https://x/#f', 'not a url']) {
    assert.throws(() => new SteelmatterClient({ baseUrl, token: 't' }), TypeError, baseUrl);
  }
});
