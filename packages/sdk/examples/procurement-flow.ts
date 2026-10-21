// Example: one purchase order through the Steelmatter R0 API with the SDK.
//
// Prerequisites (see docs/quickstart.md): a running service, synthetic
// principals created by `smdemo setup`, and a mandate whose synthetic-erp
// workflow is assigned to agent-a (trusted operator configuration).
//
//   STEELMATTER_SERVER=http://127.0.0.1:8080 \
//   STEELMATTER_TOKENS=.local/demo/tokens.json \
//   STEELMATTER_MANDATE=<mandate id> \
//   node packages/sdk/examples/procurement-flow.ts
//
// Simulation with synthetic data only.
import { readFileSync } from 'node:fs';
import { SteelmatterClient, SteelmatterApiError, isPermitIssued, formatINR, amountMinor, type ProposedAction } from '../src/index.ts';

const baseUrl = process.env.STEELMATTER_SERVER ?? 'http://127.0.0.1:8080';
const tokens: Record<string, string> = JSON.parse(readFileSync(process.env.STEELMATTER_TOKENS ?? '.local/demo/tokens.json', 'utf8'));
const mandateId = process.env.STEELMATTER_MANDATE;
if (!mandateId) throw new Error('set STEELMATTER_MANDATE to a mandate assigned to agent-a');

// One client per principal: identity always comes from the credential.
const issuer = new SteelmatterClient({ baseUrl, token: tokens['issuer-a']! });
const agent = new SteelmatterClient({ baseUrl, token: tokens['agent-a']! });
const observer = new SteelmatterClient({ baseUrl, token: tokens['observer-a']! });

const proposal: ProposedAction = {
  supplier_id: 'supplier-a', delivery_id: 'warehouse-a', currency: 'INR',
  lines: [{ item_id: 'item-a', quantity: 3, unit_price_minor: 20_000_000 }],
};
console.log(`proposing ${formatINR(amountMinor(proposal.lines))}`);

// 1. The trusted issuer records the source instruction (safe to repeat).
const instruction = await issuer.createInstruction({
  source_system: 'synthetic-erp', source_record_id: `example-${Date.now()}`, mandate_id: mandateId, proposed_action: proposal,
});
console.log(`instruction ${instruction.data.instruction_id} → actor ${instruction.data.actor_id}`);

// 2. The assigned actor asks for a permit. A denial is a normal 200 result.
const evaluation = await agent.evaluateAction({ instruction_id: instruction.data.instruction_id, action_revision: instruction.data.action_revision });
if (!isPermitIssued(evaluation)) {
  console.log(`denied: ${evaluation.data.reason}`);
  process.exit(0);
}
const { action, permit_jws: permitJws, permit } = evaluation.data;
console.log(`permit ${permit.permit_id} valid until ${permit.expires_at}`);

// 3. Present the exact action and permit. If the response is lost, repeating
//    the identical call returns the stored result without a second order.
try {
  const op = await agent.admitOperation({ action, permit_jws: permitJws });
  console.log(`${op.created ? 'committed' : 'already committed'}: order ${op.data.target_ref.order_id}, obligation ${op.data.obligation_id}`);

  // 4. Read back and fetch evidence (verify it with @irontrex/steelmatter-verifier).
  const rec = await observer.reconcileOperation(op.data.operation_id);
  const ev = await observer.getEvidence(op.data.operation_id);
  console.log(`observation ${rec.data.observation}; evidence ${ev.data.evidence}; incomplete [${ev.data.incomplete.join(', ')}]`);
} catch (e) {
  if (e instanceof SteelmatterApiError) {
    console.error(`${e.code} (HTTP ${e.status}, request ${e.requestId})${e.operationId ? `; look up operation ${e.operationId}` : ''}`);
    process.exit(1);
  }
  throw e;
}
