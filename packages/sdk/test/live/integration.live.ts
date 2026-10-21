// Live integration test: the SDK against the real Steelmatter service and
// PostgreSQL. It is started by tests/integration/sdk_test.go (Go), which
// provisions the database, keys and synthetic principals, then runs this file
// with:
//   SM_SDK_BASE_URL   service URL
//   SM_SDK_TOKENS     JSON map of principal id → bearer token
//   SM_SDK_MANDATE    mandate whose synthetic-erp workflow is assigned to agent-a
//   SM_SDK_TRUST      trust-store path for offline evidence verification
//   SM_SDK_SCENARIO   "flow" (all 11 operations) or "quota" (429 handling)
// It is not part of `npm test` because it needs that environment.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  SteelmatterClient, SteelmatterApiError, isPermitIssued, isDenied, amountMinor, type ProposedAction, type ApiResponse,
  type EvaluateApiResponse, type PermitIssued,
} from '../../src/index.ts';

// The standalone verifier (packages/verifier) checks the evidence the SDK
// fetched. Imported by path at runtime so the SDK typecheck does not compile
// the verifier's sources under the SDK's settings.
interface VerifierReport {
  linkage: { status: string };
  observation: { status: string };
  records: Record<string, { integrity: string; issuer: string }>;
}
const verifierPath = new URL('../../../verifier/src/index.ts', import.meta.url).href;
const { loadTrustStore, verifyBundle } = (await import(verifierPath)) as {
  loadTrustStore(doc: Uint8Array): Promise<unknown>;
  verifyBundle(bundle: unknown, trust: unknown): Promise<VerifierReport>;
};

const env = (k: string) => {
  const v = process.env[k];
  if (!v) throw new Error(`${k} is not set; run through tests/integration/sdk_test.go`);
  return v;
};
const baseUrl = env('SM_SDK_BASE_URL');
const tokens: Record<string, string> = JSON.parse(env('SM_SDK_TOKENS'));
const scenario = env('SM_SDK_SCENARIO');
const as = (who: string, extra: object = {}) => new SteelmatterClient({ baseUrl, token: tokens[who]!, timeoutMs: 15_000, ...extra });
const run = `${process.pid}-${Date.now()}`;

async function apiError(p: Promise<unknown>): Promise<SteelmatterApiError> {
  const e = await p.then(() => undefined, (err: unknown) => err);
  assert.ok(e instanceof SteelmatterApiError, `expected SteelmatterApiError, got ${String(e)}`);
  return e;
}

function permit(r: EvaluateApiResponse): PermitIssued {
  assert.ok(isPermitIssued(r), `expected a permit, got ${JSON.stringify(r.data)}`);
  return r.data;
}

const sixLakh = (supplier = 'supplier-a'): ProposedAction => ({
  supplier_id: supplier, delivery_id: 'warehouse-a', currency: 'INR', lines: [{ item_id: 'item-a', quantity: 3, unit_price_minor: 20_000_000 }],
});

if (scenario === 'flow') {
  const mandateId = env('SM_SDK_MANDATE');
  const owner = as('owner-a');
  const issuer = as('issuer-a');
  const agent = as('agent-a');
  const otherAgent = as('agent-b');
  const observer = as('observer-a');
  const operator = as('operator-a');

  test('mandates: create (201) and idempotent revoke (200)', async () => {
    const created = await owner.createMandate({
      actor_ids: ['agent-b'], audience: 'synthetic-procurement', supplier_ids: ['supplier-b'], delivery_ids: ['warehouse-a'],
      currency: 'INR', limit_minor: 50_000_000, expires_at: new Date(Date.now() + 3_600_000).toISOString().slice(0, 19) + 'Z',
    });
    assert.equal(created.status, 201);
    assert.equal(created.data.status, 'ACTIVE');
    assert.equal(created.data.mandate.revision, 1);
    assert.equal(created.data.mandate.issuer_id, 'owner-a', 'the server derives the owner from the credential');
    assert.deepEqual(created.data.buckets, { limit_minor: 50_000_000, available_minor: 50_000_000, reserved_minor: 0, committed_minor: 0, unresolved_minor: 0 });
    assert.match(created.data.mandate_jws, /^[\w-]+\.[\w-]+\.[\w-]+$/);
    const id = created.data.mandate.mandate_id;
    const first = await owner.revokeMandate(id);
    const again = await owner.revokeMandate(id);
    assert.equal(first.status, 200);
    assert.equal(again.status, 200);
    assert.equal(first.data.status, 'REVOKED');
    assert.equal(again.data.control_epoch, first.data.control_epoch, 'repeat revocation does not bump the epoch');
    assert.equal((await apiError(agent.createMandate({ ...created.data.mandate, actor_ids: ['agent-a'] } as never))).status, 400,
      'server-owned fields are rejected');
  });

  let instructionId = '';
  test('instructions: create (201), identical repeat (200), changed repeat conflicts', async () => {
    const body = { source_system: 'synthetic-erp', source_record_id: `po-${run}`, mandate_id: mandateId, proposed_action: sixLakh(), client_request_id: 'corr-1' };
    const created = await issuer.createInstruction(body);
    assert.equal(created.status, 201);
    assert.equal(created.data.actor_id, 'agent-a', 'the actor comes from the trusted assignment, not the issuer');
    assert.equal(created.data.action.amount_minor, amountMinor(body.proposed_action.lines));
    instructionId = created.data.instruction_id;
    const repeat = await issuer.createInstruction({ ...body, client_request_id: 'corr-2' });
    assert.equal(repeat.status, 200);
    assert.equal(repeat.data.instruction_id, instructionId);
    const conflict = await apiError(issuer.createInstruction({ ...body, proposed_action: sixLakh('supplier-b') }));
    assert.equal(conflict.status, 409);
    assert.equal(conflict.code, 'SOURCE_CONFLICT');
    const forged = await apiError(agent.createInstruction({ ...body, source_record_id: `forged-${run}` }));
    assert.equal(forged.code, 'FORBIDDEN');
  });

  test('actions: simulate creates nothing; evaluate issues once; the wrong actor is refused', async () => {
    const sim = await operator.simulateAction({ instruction_id: instructionId, action_revision: 1 });
    assert.deepEqual(sim.data, { simulation: true, decision: 'PERMIT_WOULD_ISSUE' });
    const wrong = await apiError(otherAgent.evaluateAction({ instruction_id: instructionId, action_revision: 1 }));
    assert.equal(wrong.code, 'ACTOR_MISMATCH');
    const issued = await agent.evaluateAction({ instruction_id: instructionId, action_revision: 1 });
    assert.equal(issued.status, 201);
    const retry = await agent.evaluateAction({ instruction_id: instructionId, action_revision: 1 });
    assert.equal(retry.status, 200);
    assert.equal(permit(retry).permit.permit_id, permit(issued).permit.permit_id, 'a retry returns the same issuance');
    assert.equal(permit(retry).permit.expires_at, permit(issued).permit.expires_at, 'a retry never renews');
  });

  test('actions: capacity denial is a 200 DENY; an amendment requires re-evaluation', async () => {
    const second = await issuer.createInstruction({ source_system: 'synthetic-erp', source_record_id: `po2-${run}`, mandate_id: mandateId, proposed_action: sixLakh() });
    const denied = await agent.evaluateAction({ instruction_id: second.data.instruction_id, action_revision: 1 });
    assert.equal(denied.status, 200);
    assert.ok(isDenied(denied) && denied.data.reason === 'CAPACITY_EXHAUSTED');
    const amendment = { amendment_id: `am-${run}`, expected_action_revision: 1, proposed_action: { ...sixLakh(), lines: [{ item_id: 'item-a', quantity: 1, unit_price_minor: 10_000_000 }] } };
    const amended = await issuer.amendInstruction(second.data.instruction_id, amendment);
    assert.equal(amended.status, 201);
    assert.equal(amended.data.action_revision, 2);
    const retried = await issuer.amendInstruction(second.data.instruction_id, amendment);
    assert.equal(retried.status, 200);
    const stale = await apiError(issuer.amendInstruction(second.data.instruction_id, { ...amendment, amendment_id: `am2-${run}` }));
    assert.equal(stale.code, 'REVISION_STALE');
    const old = await apiError(agent.evaluateAction({ instruction_id: second.data.instruction_id, action_revision: 1 }));
    assert.equal(old.code, 'REVISION_STALE');
    const sim = await operator.simulateAction({ instruction_id: second.data.instruction_id, action_revision: 2 });
    assert.equal(sim.data.decision, 'PERMIT_WOULD_ISSUE');
  });

  let operationId = '';
  let obligationId = '';
  test('operations: tampering conflicts; admit commits once; a repeat returns the stored result', async () => {
    const issued = permit(await agent.evaluateAction({ instruction_id: instructionId, action_revision: 1 }));
    const switched = await apiError(agent.admitOperation({ action: { ...issued.action, supplier_id: 'supplier-b' }, permit_jws: issued.permit_jws }));
    assert.equal(switched.code, 'ACTION_CHANGED');
    const inconsistent = await apiError(agent.admitOperation({ action: { ...issued.action, amount_minor: issued.action.amount_minor + 1 }, permit_jws: issued.permit_jws }));
    assert.equal(inconsistent.code, 'AMOUNT_MISMATCH', 'the total must equal the lines');
    const committed = await agent.admitOperation({ action: issued.action, permit_jws: issued.permit_jws });
    assert.equal(committed.status, 201);
    assert.equal(committed.data.execution, 'COMMITTED');
    operationId = committed.data.operation_id;
    obligationId = committed.data.obligation_id;
    const repeat = await agent.admitOperation({ action: issued.action, permit_jws: issued.permit_jws });
    assert.equal(repeat.status, 200);
    assert.deepEqual(repeat.data, committed.data, 'the identical result, no second effect');
    const status = await agent.getOperation(operationId);
    assert.equal(status.data.execution, 'COMMITTED');
    assert.equal(status.data.authorisation, 'PERMITTED');
    assert.equal(status.data.target_ref?.order_id, committed.data.target_ref.order_id);
  });

  test('operations: reconcile and evidence verify offline with explicit trust anchors', async () => {
    const rec = await observer.reconcileOperation(operationId);
    assert.equal(rec.status, 200);
    assert.deepEqual(rec.data, { operation_id: operationId, state: 'RESOLVED', observation: 'MATCH' });
    const ev = await observer.getEvidence(operationId);
    assert.equal(ev.data.evidence, 'AVAILABLE');
    assert.deepEqual(ev.data.incomplete, []);
    assert.equal(ev.data.links.obligation_id, obligationId);
    const trust = await loadTrustStore(new Uint8Array(readFileSync(env('SM_SDK_TRUST'))));
    const report = await verifyBundle(ev.data, trust);
    assert.equal(report.linkage.status, 'COMPLETE');
    assert.equal(report.observation.status, 'MATCH');
    assert.ok(Object.values(report.records).every((r) => r.integrity === 'VALID' && r.issuer === 'TRUSTED'));
  });

  test('adapters: assurance for a reader; unknown adapter is 404', async () => {
    const a = await agent.getAdapterAssurance('synthetic-procurement');
    assert.equal(a.data.simulation, true);
    assert.deepEqual(a.data.supported_operations, ['purchase_order.issue']);
    assert.ok(a.data.limitations.length > 0 && a.data.test_references.length > 0);
    assert.equal((await apiError(agent.getAdapterAssurance('no-such-adapter'))).status, 404);
  });

  test('errors: bad credential 401, foreign scope 404 without disclosure', async () => {
    const anon = new SteelmatterClient({ baseUrl, token: 'not-a-real-token' });
    const unauth = await apiError(anon.getOperation(operationId));
    assert.equal(unauth.status, 401);
    assert.equal(unauth.code, 'UNAUTHENTICATED');
    assert.match(unauth.requestId, /^req_[a-f0-9]{24}$/);
    const foreign = await apiError(as('agent-x').getOperation(operationId));
    assert.equal(foreign.status, 404);
    assert.equal(foreign.code, 'NOT_FOUND');
  });
} else if (scenario === 'quota') {
  test('429: the SDK waits for Retry-After and retries; retry:false surfaces RATE_LIMITED', async () => {
    const owner = as('owner-a');
    await owner.getAdapterAssurance('synthetic-procurement'); // uses the burst of one
    const started = Date.now();
    const r = await owner.getAdapterAssurance('synthetic-procurement');
    assert.equal(r.status, 200);
    assert.equal(r.attempts, 2);
    assert.ok(Date.now() - started >= 900, 'waited for Retry-After');
    const e = await apiError(owner.getAdapterAssurance('synthetic-procurement', { retry: false }));
    assert.equal(e.status, 429);
    assert.equal(e.code, 'RATE_LIMITED');
    assert.equal(e.retryable, true);
    assert.equal(e.retryAfterMs, 1000);
  });
} else {
  throw new Error(`unknown SM_SDK_SCENARIO ${scenario}`);
}
