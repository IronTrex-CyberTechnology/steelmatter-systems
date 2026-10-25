import { canonical, digest, parseJSON, snapshot } from './json.mjs';

export const MANIFEST_SCHEMA = 'steelmatter.destination-manifest/0.1';
const ID = /^[a-z][a-z0-9._-]{0,95}$/;
const FIELD = /^[a-z][a-z0-9_]{0,63}$/;
const FORBIDDEN = new Set(['constructor', 'prototype', '__proto__']);

export function fail(message) { throw new TypeError(message); }
export function object(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) fail(`${label}: expected object`);
  return value;
}
export function closed(value, keys, label) {
  object(value, label);
  const actual = Object.keys(value);
  if (actual.length !== keys.length || actual.some(k => !keys.includes(k))) fail(`${label}: wrong or missing fields`);
}
export function choice(value, choices, label) {
  if (!choices.includes(value)) fail(`${label}: unsupported value`);
}
export function identifier(value, label) {
  if (typeof value !== 'string' || !ID.test(value)) fail(`${label}: invalid identifier`);
}
export function nonempty(value, label, max = 256) {
  if (typeof value !== 'string' || value.length < 1 || value.length > max || /[\u0000-\u001f\u007f]/u.test(value)) fail(`${label}: invalid text`);
}

// This validates declarations, not implementations. Even a passing declaration
// is NOT an admission decision, certification, or proof of destination behavior.
export function validateManifest(input) {
  const m = snapshot(input);
  closed(m, ['schema', 'adapter_id', 'adapter_version', 'environment', 'operations', 'limitations'], 'manifest');
  choice(m.schema, [MANIFEST_SCHEMA], 'schema');
  identifier(m.adapter_id, 'adapter_id');
  if (typeof m.adapter_version !== 'string' || !/^\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?$/.test(m.adapter_version)) fail('adapter_version: expected version');
  choice(m.environment, ['synthetic', 'external'], 'environment');
  if (!Array.isArray(m.operations) || m.operations.length < 1 || m.operations.length > 32) fail('operations: expected 1..32 entries');
  const seen = new Set();
  for (const op of m.operations) {
    closed(op, ['name', 'effect', 'parameters', 'precondition', 'idempotency', 'reconciliation', 'observation'], 'operation');
    identifier(op.name, 'operation.name');
    if (seen.has(op.name)) fail('operation.name: duplicate');
    seen.add(op.name);
    choice(op.effect, ['non-payment'], 'effect');
    object(op.parameters, 'parameters');
    const names = Object.keys(op.parameters);
    if (!names.length || names.length > 32) fail('parameters: expected 1..32 fields');
    for (const name of names) {
      if (!FIELD.test(name) || FORBIDDEN.has(name)) fail('parameters: unsafe field name');
      choice(op.parameters[name], ['string', 'integer', 'boolean'], `parameters.${name}`);
    }
    choice(op.precondition, ['atomic-version-check', 'unsupported'], 'precondition');
    choice(op.idempotency, ['atomic-operation-journal', 'unsupported'], 'idempotency');
    choice(op.reconciliation, ['operation-journal-read', 'unsupported'], 'reconciliation');
    choice(op.observation, ['separate-read-path', 'unsupported'], 'observation');
  }
  if (!Array.isArray(m.limitations) || !m.limitations.length || m.limitations.length > 32) fail('limitations: expected 1..32 entries');
  for (const line of m.limitations) nonempty(line, 'limitation', 1024);
  return m;
}

export function parseManifest(bytes) { return validateManifest(parseJSON(bytes)); }
export function manifestDigest(manifest) { return digest('manifest', validateManifest(manifest)); }

export function inspectManifest(input) {
  const m = validateManifest(input);
  const blockers = [];
  for (const op of m.operations) {
    for (const feature of ['precondition', 'idempotency', 'reconciliation', 'observation']) {
      if (op[feature] === 'unsupported') blockers.push(`${op.name}: ${feature} unsupported`);
    }
  }
  return snapshot({
    schema: m.schema,
    adapter_id: m.adapter_id,
    manifest_digest: digest('manifest', m),
    declaration: blockers.length ? 'BLOCKED' : 'CAPABILITIES_DECLARED',
    blockers,
    implementation_verified: false,
    authorization_granted: false,
  });
}

export function scaffold(adapterId) {
  identifier(adapterId, 'adapter_id');
  const manifest = validateManifest({
    schema: MANIFEST_SCHEMA,
    adapter_id: adapterId,
    adapter_version: '0.1.0-draft',
    environment: 'synthetic',
    operations: [{
      name: 'record.set-enabled', effect: 'non-payment', parameters: { enabled: 'boolean' },
      precondition: 'unsupported', idempotency: 'unsupported',
      reconciliation: 'unsupported', observation: 'unsupported',
    }],
    limitations: ['Unimplemented scaffold. Not authorized for real actions.'],
  });
  const source = `// Apache-2.0. Generated scaffold; no execution implementation.\nexport function createAdapter() {\n  const unavailable = async () => { throw new Error('ADAPTER_NOT_IMPLEMENTED'); };\n  return Object.freeze({\n    precheck: unavailable,\n    execute: unavailable,\n    observe: unavailable,\n    reconcile: unavailable,\n  });\n}\n`;
  return snapshot({ 'manifest.json': `${canonical(manifest)}\n`, 'adapter.mjs': source });
}
