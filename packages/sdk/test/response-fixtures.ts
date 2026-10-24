// Complete synthetic responses for transport tests; signed fixture material
// comes from the existing public vectors. Structural validity is not trust.
import { readFileSync } from 'node:fs';
const packet = (kind: string) => JSON.parse(readFileSync(new URL(`../../../fixtures/packet/${kind}.payload.json`, import.meta.url), 'utf8'));
const vectors = JSON.parse(readFileSync(new URL('../../../fixtures/vectors/evidence-v1.json', import.meta.url), 'utf8'));
const evidence = vectors.cases.find((c: { name: string }) => c.name === 'complete').bundle;
const digest = 'a'.repeat(64);
const buckets = { limit_minor: 100, available_minor: 100, reserved_minor: 0, committed_minor: 0, unresolved_minor: 0 };
export const responseFixtures: Record<string, any> = {
  createMandate: { mandate: packet('mandate'), mandate_digest: digest, mandate_jws: 'a.b.c', status: 'ACTIVE', buckets },
  revokeMandate: { mandate_id: 'm-1', status: 'REVOKED', control_epoch: 2, revoked_at: '2026-10-09T10:00:00Z', buckets },
  createInstruction: { instruction_id: 'i', source_system: 'erp', source_record_id: 'r', mandate_id: 'm-1', actor_id: 'agent-a', action_revision: 1, action: packet('action'), action_digest: digest, status: 'OPEN' },
  amendInstruction: { amendment_id: 'am-1', instruction_id: 'i', previous_action_revision: 1, action_revision: 2, action: packet('action'), action_digest: digest, invalidated_permit_ids: [], released_minor: 0 },
  evaluateAction: { decision: 'PERMIT_ISSUED', permit: packet('permit'), permit_jws: 'a.b.c', permit_digest: digest, action: packet('action'), action_digest: digest },
  simulateAction: { simulation: true, decision: 'PERMIT_WOULD_ISSUE' },
  admitOperation: { operation_id: 'op-1', instruction_id: 'i', action_revision: 1, action_digest: digest, execution: 'COMMITTED', obligation_id: 'obl-1', target_ref: { adapter_id: 'a', order_id: 'o' }, committed_at: '2026-10-09T10:00:00Z' },
  getOperation: { operation_id: 'op', instruction_id: 'i', authorisation: 'PERMITTED', execution: 'COMMITTED', observation: 'NOT_OBSERVED', evidence: 'PENDING' },
  reconcileOperation: { operation_id: 'op', state: 'RESOLVED', observation: 'MATCH' },
  getEvidence: evidence,
  getAdapterAssurance: { adapter_id: 'a', version: '1', simulation: true, supported_operations: ['purchase_order.issue'], supported_paths: [], limitations: [], test_references: [] },
};
