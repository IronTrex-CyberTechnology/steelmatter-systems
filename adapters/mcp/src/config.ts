import { readFileSync, statSync } from 'node:fs';

export interface AdapterConfig {
  baseUrl: string;
  token: string;
  timeoutMs?: number;
}

/**
 * Reads the adapter configuration from the environment:
 *   STEELMATTER_SERVER          service base URL (default http://127.0.0.1:8080)
 *   STEELMATTER_MCP_TOKEN_FILE  file holding the actor's bearer token (owner-only permissions), or
 *   STEELMATTER_MCP_TOKEN       the token itself (the file is preferred: MCP client configs are often shared)
 *   STEELMATTER_MCP_TIMEOUT_MS  optional per-attempt timeout
 * Configure the token of the one actor this agent acts as. Never configure an
 * owner, issuer, observer or operator token: the agent must not hold them.
 */
export function loadConfig(env: NodeJS.ProcessEnv): AdapterConfig {
  const baseUrl = env.STEELMATTER_SERVER || 'http://127.0.0.1:8080';
  const file = env.STEELMATTER_MCP_TOKEN_FILE;
  const direct = env.STEELMATTER_MCP_TOKEN;
  if (file && direct) throw new Error('set only one of STEELMATTER_MCP_TOKEN_FILE and STEELMATTER_MCP_TOKEN');
  let token: string;
  if (file) {
    const st = statSync(file);
    if (!st.isFile()) throw new Error('STEELMATTER_MCP_TOKEN_FILE is not a regular file');
    if (process.platform !== 'win32' && (st.mode & 0o077) !== 0) {
      throw new Error('STEELMATTER_MCP_TOKEN_FILE must be readable by its owner only (chmod 600)');
    }
    token = readFileSync(file, 'utf8').trim();
  } else if (direct) {
    token = direct.trim();
  } else {
    throw new Error('STEELMATTER_MCP_TOKEN_FILE (or STEELMATTER_MCP_TOKEN) is required');
  }
  if (token === '' || /\s/.test(token)) throw new Error('the configured token is empty or contains whitespace');
  const cfg: AdapterConfig = { baseUrl, token };
  if (env.STEELMATTER_MCP_TIMEOUT_MS) {
    const n = Number(env.STEELMATTER_MCP_TIMEOUT_MS);
    if (!Number.isSafeInteger(n) || n <= 0) throw new Error('STEELMATTER_MCP_TIMEOUT_MS must be a positive integer');
    cfg.timeoutMs = n;
  }
  return cfg;
}
