import { digest, snapshot } from './json.mjs';
import { closed, fail, nonempty, validateManifest } from './manifest.mjs';

const ACTION_FIELDS = ['operation_id', 'tenant_id', 'actor_id', 'destination_id', 'operation', 'target_id', 'expected_version', 'parameters'];
const BOUND_FIELDS = ['schema', 'manifest_digest', ...ACTION_FIELDS, 'action_digest'];
const SCHEMA = 'steelmatter.destination-action/0.1';

// This is a content binding, NOT a permit. Anyone can calculate these hashes.
// A trusted authority must separately admit this exact action and adapter code.
export function bindAction(manifest, request) {
  const m = validateManifest(manifest);
  const input = snapshot(request);
  closed(input, ACTION_FIELDS, 'action');
  for (const field of ACTION_FIELDS.filter(f => f !== 'parameters')) nonempty(input[field], field);
  const op = m.operations.find(o => o.name === input.operation);
  if (!op) fail('operation: not declared');
  closed(input.parameters, Object.keys(op.parameters), 'parameters');
  for (const [name, kind] of Object.entries(op.parameters)) {
    const value = input.parameters[name];
    if (kind === 'integer' ? !Number.isSafeInteger(value) : typeof value !== kind) fail(`parameters.${name}: wrong type`);
    if (kind === 'string') nonempty(value, `parameters.${name}`, 4096);
  }
  const action = { schema: SCHEMA, manifest_digest: digest('manifest', m), ...input };
  return snapshot({ ...action, action_digest: digest('action', action) });
}

export function verifyBoundAction(manifest, candidate) {
  const value = snapshot(candidate);
  closed(value, BOUND_FIELDS, 'bound action');
  const request = Object.fromEntries(ACTION_FIELDS.map(k => [k, value[k]]));
  const expected = bindAction(manifest, request);
  if (value.schema !== SCHEMA || value.manifest_digest !== expected.manifest_digest || value.action_digest !== expected.action_digest) fail('ACTION_BINDING_MISMATCH');
  return expected;
}

// An execution acknowledgement is not an observation. Compare the material
// readback as well as the operation/target/version linkage. Transport and
// credential separation remain the adapter author's responsibility.
export function compareObservation(manifest, candidate, execution, observation) {
  const action = verifyBoundAction(manifest, candidate);
  let e, o;
  try {
    e = snapshot(execution);
    o = snapshot(observation);
    closed(e, ['status', 'operation_id', 'action_digest', 'target_version'], 'execution');
    if (e.status !== 'APPLIED') return 'INSUFFICIENT_EVIDENCE';
    nonempty(e.target_version, 'execution.target_version');
    if (o === null) return 'INSUFFICIENT_EVIDENCE';
    closed(o, ['operation_id', 'action_digest', 'tenant_id', 'destination_id', 'target_id', 'target_version', 'values'], 'observation');
    nonempty(o.target_version, 'observation.target_version');
    closed(o.values, Object.keys(action.parameters), 'observation.values');
  } catch {
    return 'INSUFFICIENT_EVIDENCE';
  }
  for (const field of ['operation_id', 'action_digest']) {
    if (e[field] !== action[field] || o[field] !== action[field]) return 'MISMATCH';
  }
  for (const field of ['tenant_id', 'destination_id', 'target_id']) {
    if (o[field] !== action[field]) return 'MISMATCH';
  }
  if (e.target_version !== o.target_version || e.target_version === action.expected_version) return 'MISMATCH';
  for (const [key, value] of Object.entries(action.parameters)) {
    if (o.values[key] !== value) return 'MISMATCH';
  }
  return 'MATCH';
}
