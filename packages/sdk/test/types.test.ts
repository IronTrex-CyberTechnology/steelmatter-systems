// Compile-time checks of the public types, enforced by `npm run typecheck`
// (tsc fails if an @ts-expect-error line stops being an error). The function
// is never called at runtime; the test below only proves the file loads.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SteelmatterClient, isPermitIssued, isDenied, type ErrorCode, type SteelmatterApiError } from '../src/index.ts';

async function typeChecks(c: SteelmatterClient, err: SteelmatterApiError): Promise<void> {
  const proposal = { supplier_id: 's', delivery_id: 'd', currency: 'INR' as const, lines: [{ item_id: 'i', quantity: 1, unit_price_minor: 1 }] };
  // @ts-expect-error currency is INR only
  await c.createInstruction({ source_system: 's', source_record_id: 'r', mandate_id: 'm', proposed_action: { ...proposal, currency: 'USD' } });
  // @ts-expect-error server-owned identity fields are not part of the request type
  await c.createInstruction({ source_system: 's', source_record_id: 'r', mandate_id: 'm', proposed_action: proposal, actor_id: 'agent-a' });
  // @ts-expect-error limit_minor is required
  await c.createMandate({ actor_ids: ['a'], audience: 'x', supplier_ids: ['s'], delivery_ids: ['d'], currency: 'INR', expires_at: 'x' });
  // @ts-expect-error there is no destination field on admission
  await c.admitOperation({ action: {} as never, permit_jws: 'a.b.c', destination_url: 'https://evil' });

  const r = await c.evaluateAction({ instruction_id: 'i', action_revision: 1 });
  // @ts-expect-error permit_jws is only present after narrowing
  void r.data.permit_jws;
  if (isPermitIssued(r)) {
    const jws: string = r.data.permit_jws;
    void jws;
  } else {
    const reason: 'AUTHORITY_REVOKED' | 'CAPACITY_EXHAUSTED' | 'DELIVERY_NOT_PERMITTED' | 'MANDATE_EXPIRED' | 'SUPPLIER_NOT_PERMITTED' = r.data.reason;
    void reason;
  }
  if (isDenied(r)) void r.data.message;

  const code: ErrorCode = err.code;
  // @ts-expect-error codes are a closed union
  const bogus: ErrorCode = 'NOT_A_CODE';
  void code; void bogus;
}

test('type checks compile (enforced by tsc)', () => {
  assert.equal(typeof typeChecks, 'function');
});
