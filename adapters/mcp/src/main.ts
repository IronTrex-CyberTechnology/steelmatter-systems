#!/usr/bin/env node
// steelmatter-mcp: the Steelmatter thin MCP adapter over stdio.
// stdout carries only newline-delimited JSON-RPC messages; diagnostics go to
// stderr and never include the credential. Configuration: see config.ts.
import { createInterface } from 'node:readline';
import { SteelmatterClient } from '@irontrex/steelmatter-sdk';
import { loadConfig } from './config.ts';
import { McpServer } from './server.ts';

const MAX_LINE = 16 << 20;
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
const rl = createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on('line', (line) => {
  if (line.trim() === '') return;
  if (line.length > MAX_LINE) {
    process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request: message too large' } }) + '\n');
    return;
  }
  const p = server.handleLine(line).then((out) => {
    if (out !== undefined) process.stdout.write(out + '\n');
  }, (e: unknown) => {
    log(`unexpected failure: ${(e as Error)?.message ?? e}`);
  });
  pending.add(p);
  void p.finally(() => pending.delete(p));
});
rl.on('close', () => {
  void Promise.all(pending).then(() => process.exit(0));
});
