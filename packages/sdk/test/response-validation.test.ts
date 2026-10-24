import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { SteelmatterClient, SteelmatterResponseError, SteelmatterApiError } from '../src/index.ts';
import { schemaSources } from '../src/response-schemas.ts';
import { responseFixtures } from './response-fixtures.ts';

function probe(body: unknown, status: number) {
  let calls = 0;
  const client = new SteelmatterClient({ baseUrl: 'https://synthetic.invalid', token: 'synthetic',
    retry: { maxAttempts: 3, baseDelayMs: 0, maxDelayMs: 0 },
    fetch: async () => { calls++; return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json', 'x-request-id': 'req_' + 'a'.repeat(24) } }); } });
  return { client, calls: () => calls };
}
const invoke = (c: SteelmatterClient, op: string) => (c as any)[op]('synthetic', {});

test('response schema snapshot tracks the API and signed payload contracts', () => {
  for (const [path, hash] of Object.entries(schemaSources)) {
    const content = readFileSync(new URL('../../../' + path, import.meta.url));
    assert.equal(createHash('sha256').update(content).digest('hex'), hash, `regenerate response schemas after changing ${path}`);
  }
});

for (const [op, valid] of Object.entries(responseFixtures)) {
  const status = op === 'createMandate' ? 201 : 200;
  test(`${op}: valid response accepted; empty, missing, mistyped and extra fields fail without retry`, async () => {
    const control = probe(valid, status);
    assert.deepEqual((await invoke(control.client, op)).data, valid);
    const mutations: unknown[] = [{}, [], null, {...valid, unexpected: true}];
    // Required top-level fields in each of these fixtures (target_ref and reason are absent).
    for (const key of Object.keys(valid)) {
      const missing = structuredClone(valid); delete missing[key]; mutations.push(missing);
      mutations.push({...valid, [key]: null});
    }
    for (const body of mutations) {
      const p = probe(body, status);
      await assert.rejects(invoke(p.client, op), SteelmatterResponseError, `${op}: ${JSON.stringify(body)}`);
      assert.equal(p.calls(), 1);
    }
  });
}

test('malformed retryable envelopes cannot be rescued by a valid header request ID', async () => {
  const valid = {error_code: 'SERVICE_UNAVAILABLE', message: 'synthetic', request_id: 'req_' + 'b'.repeat(24), retryable: true};
  const {request_id: _, ...missing} = valid;
  for (const body of [missing, ...[42, null, '', 'req_invalid'].map(request_id => ({...valid, request_id})),
    {...valid, operation_id: 4}, {...valid, unexpected: true}, {...valid, message: 'x'.repeat(1025)}]) {
    const p = probe(body, 503);
    await assert.rejects(p.client.getOperation('op'), SteelmatterResponseError);
    assert.equal(p.calls(), 1);
  }
  const p = probe(valid, 503);
  await assert.rejects(p.client.getOperation('op'), SteelmatterApiError);
  assert.equal(p.calls(), 3);
});

test('nested violations, unsafe integers and impossible timestamps are rejected', async () => {
  for (const [op, body] of [
    ['admitOperation', {...responseFixtures.admitOperation, target_ref: {adapter_id: 'a'}}],
    ['admitOperation', {...responseFixtures.admitOperation, target_ref: {adapter_id: 'a', order_id: 'o', destination: 'attacker'}}],
    ['admitOperation', {...responseFixtures.admitOperation, action_revision: 9007199254740992}],
    ['admitOperation', {...responseFixtures.admitOperation, committed_at: '2026-02-30T00:00:00Z'}],
    ['getEvidence', {...responseFixtures.getEvidence, records: {...responseFixtures.getEvidence.records, invoice_jws: 'a.b.c'}}],
    ['getOperation', {...responseFixtures.getOperation, execution: 'INVENTED'}],
  ] as const) {
    const p = probe(body, 200);
    await assert.rejects(invoke(p.client, op), SteelmatterResponseError); assert.equal(p.calls(), 1);
  }
});

test('DENY is allowed at 200, never at 201', async () => {
  const body = {decision: 'DENY', reason: 'CAPACITY_EXHAUSTED', message: 'denied', instruction_id: 'i', action_revision: 1};
  assert.equal((await probe(body, 200).client.evaluateAction({instruction_id:'i',action_revision:1})).data.decision, 'DENY');
  await assert.rejects(probe(body, 201).client.evaluateAction({instruction_id:'i',action_revision:1}), SteelmatterResponseError);
});

test('reconciliation rejects the 202 status reserved for a future release', async () => {
  const p = probe(responseFixtures.reconcileOperation, 202);
  await assert.rejects(p.client.reconcileOperation('op'), SteelmatterResponseError);
  assert.equal(p.calls(), 1);
});
