// Apache-2.0. Synthetic, process-local test fixture, never a real connector.
import { MANIFEST_SCHEMA, bindAction, validateManifest, verifyBoundAction } from '../src/index.mjs';
import { snapshot } from '../src/json.mjs';

export const manifest = validateManifest({
  schema: MANIFEST_SCHEMA,
  adapter_id: 'synthetic-membership', adapter_version: '0.1.0', environment: 'synthetic',
  operations: [{
    name: 'member.set-enabled', effect: 'non-payment', parameters: { enabled: 'boolean' },
    precondition: 'atomic-version-check', idempotency: 'atomic-operation-journal',
    reconciliation: 'operation-journal-read', observation: 'separate-read-path',
  }],
  limitations: [
    'Synthetic data only. No authority validation, credentials, network, or real destination.',
    'Single process; synchronous check/write/journal section. State is lost on restart.',
    'Separate read function, not independent infrastructure or a separate security principal.',
    'No durable capacity ledger, cross-agent delegation, signed evidence, or payment support.',
  ],
});

export function createFixture() {
  let record = {
    tenant_id: 'tenant-a', destination_id: 'local-directory-a', target_id: 'member-1',
    version: 1, values: { enabled: true },
  };
  const journal = new Map();
  let writes = 0;
  let loseResponse = false;
  let readUnavailable = false;
  const request = snapshot({
    operation_id: 'operation-1', actor_id: 'synthetic-actor',
    tenant_id: record.tenant_id, destination_id: record.destination_id, target_id: record.target_id,
    operation: 'member.set-enabled', expected_version: '1', parameters: { enabled: false },
  });

  function bound(candidate) {
    const a = verifyBoundAction(manifest, candidate);
    for (const field of ['tenant_id', 'destination_id', 'target_id']) {
      if (a[field] !== record[field]) throw new Error('DESTINATION_SCOPE_MISMATCH');
    }
    return a;
  }
  function previous(a) {
    const prior = journal.get(a.operation_id);
    if (prior && prior.action_digest !== a.action_digest) throw new Error('OPERATION_CONFLICT');
    return prior;
  }
  const unknown = a => snapshot({ status: 'UNKNOWN', operation_id: a.operation_id, action_digest: a.action_digest });

  const adapter = Object.freeze({
    async precheck(candidate) {
      const a = bound(candidate);
      previous(a);
      if (readUnavailable) return snapshot({ status: 'UNKNOWN' });
      return snapshot({ status: a.expected_version === String(record.version) ? 'READY' : 'STALE' });
    },
    async execute(candidate) {
      const a = bound(candidate);
      const prior = previous(a);
      if (prior) return prior;
      // No await between version check, mutation and operation journal write.
      // This is atomic only within this fixture's one JavaScript process.
      if (a.expected_version !== String(record.version)) {
        const result = snapshot({ status: 'NOT_APPLIED', operation_id: a.operation_id, action_digest: a.action_digest, reason: 'STALE_VERSION' });
        journal.set(a.operation_id, result);
        return result;
      }
      record = { ...record, version: record.version + 1, values: { enabled: a.parameters.enabled } };
      writes++;
      const result = snapshot({ status: 'APPLIED', operation_id: a.operation_id, action_digest: a.action_digest, target_version: String(record.version) });
      journal.set(a.operation_id, result);
      if (loseResponse) { loseResponse = false; throw new Error('SIMULATED_LOST_RESPONSE'); }
      return result;
    },
    async reconcile(candidate) {
      const a = bound(candidate);
      if (readUnavailable) return unknown(a);
      // An absent journal record alone is NOT proof that an action cannot
      // still commit; never infer NOT_APPLIED or permission to retry from it.
      return previous(a) ?? unknown(a);
    },
    async observe(candidate) {
      const a = bound(candidate);
      if (readUnavailable) return null;
      const entry = previous(a);
      if (!entry || entry.status !== 'APPLIED') return null;
      // Read actual destination state; do not copy requested parameter values
      // or infer success from an execution acknowledgement.
      return snapshot({
        operation_id: entry.operation_id, action_digest: entry.action_digest,
        tenant_id: record.tenant_id, destination_id: record.destination_id, target_id: record.target_id,
        target_version: String(record.version), values: record.values,
      });
    },
  });
  return {
    manifest, request, adapter,
    action: () => bindAction(manifest, request),
    mutate: () => { record = { ...record, version: record.version + 1, values: { enabled: !record.values.enabled } }; },
    loseNextResponse: () => { loseResponse = true; },
    setReadUnavailable: value => { readUnavailable = value; },
    writeCount: () => writes,
  };
}
