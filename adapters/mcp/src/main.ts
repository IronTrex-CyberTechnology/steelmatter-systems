#!/usr/bin/env node
// steelmatter-mcp: the Steelmatter thin MCP adapter over stdio.
// stdout carries only newline-delimited JSON-RPC messages; diagnostics go to
// stderr and never include the credential. Configuration: see config.ts.
import { SteelmatterClient } from '@irontrex/steelmatter-sdk';
import { loadConfig } from './config.ts';
import { McpServer } from './server.ts';
import { LineFramer } from './framing.ts';

const log = (m: string) => process.stderr.write(`steelmatter-mcp: ${m}\n`);

let server: McpServer;
try {
  const cfg = loadConfig(process.env);
  const client = new SteelmatterClient({ baseUrl: cfg.baseUrl, token: cfg.token, ...(cfg.timeoutMs ? { timeoutMs: cfg.timeoutMs } : {}) });
  server = new McpServer({ client, serverInfo: { name: 'steelmatter-mcp', version: '0.0.0' }, log });
  log(`ready; service ${client.baseUrl}; simulation with synthetic data`);
} catch (e) {
  log(`configuration error: ${(e as Error).message}`);
  process.exit(2);
}

const pending = new Set<Promise<unknown>>();
const framer = new LineFramer((line) => {
  if (line.trim() === '') return;
  const p = server.handleLine(line).then((out) => {
    if (out !== undefined) process.stdout.write(out + '\n');
  }, (e: unknown) => {
    log(`unexpected failure: ${(e as Error)?.message ?? e}`);
  });
  pending.add(p);
  void p.finally(() => pending.delete(p));
}, (kind) => {
  const error = kind === 'size'
    ? { code: -32600, message: 'Invalid Request: message too large' }
    : { code: -32700, message: 'Parse error: invalid UTF-8' };
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: null, error }) + '\n');
});
process.stdin.on('data', (chunk: Buffer) => framer.push(chunk));
process.stdin.on('end', () => {
  framer.end();
  // Natural exit lets stdout drain, even when the reader applies backpressure.
  void Promise.all(pending).then(() => { process.exitCode = 0; });
});
