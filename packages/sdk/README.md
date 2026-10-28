# @irontrex/steelmatter-sdk

Typed TypeScript client for the Steelmatter R0 procurement reference API
([`api/openapi.yaml`](../../api/openapi.yaml)). It has no runtime
dependencies and uses the platform `fetch`. Private package; not published.

Simulation with synthetic data only. The SDK adds no behaviour of its own:
every decision (identity, allowance, permits, admission) is the server's.

## Install (local)
```bash
cd packages/sdk && npm ci --ignore-scripts && npm run build
npm install /path/to/Steelmatter/packages/sdk   # from your project
```
Node 26.11.0 or later (the repository pin).

## Use
```ts
import { SteelmatterClient, SteelmatterApiError, isPermitIssued } from '@irontrex/steelmatter-sdk';

// One client per principal. Tenant, actor and issuer always come from the token.
const issuer = new SteelmatterClient({ baseUrl: 'http://127.0.0.1:8080', token: process.env.ISSUER_TOKEN! });
const agent = new SteelmatterClient({ baseUrl: 'http://127.0.0.1:8080', token: process.env.AGENT_TOKEN! });

const inst = await issuer.createInstruction({
  source_system: 'synthetic-erp', source_record_id: 'po-001', mandate_id,
  proposed_action: { supplier_id: 'supplier-a', delivery_id: 'warehouse-a', currency: 'INR',
    lines: [{ item_id: 'item-a', quantity: 3, unit_price_minor: 20_000_000 }] },
});
const ev = await agent.evaluateAction({ instruction_id: inst.data.instruction_id, action_revision: inst.data.action_revision });
if (isPermitIssued(ev)) {
  const op = await agent.admitOperation({ action: ev.data.action, permit_jws: ev.data.permit_jws });
  console.log(op.created ? 'committed' : 'already committed', op.data.target_ref.order_id);
} else {
  console.log('denied:', ev.data.reason); // a policy denial is a 200 result, not an error
}
```
A complete runnable flow is in [`examples/procurement-flow.ts`](examples/procurement-flow.ts).

`baseUrl` must use `https`, except for loopback hosts (`localhost`, `127.0.0.0/8`, `[::1]`), so a bearer token never crosses a network in clear text; any other `http` URL is refused at construction. Path identifiers must be non-empty and are never `.` or `..` (a `TypeError` before any request), because URL normalisation would send those to a different route.

## Operations
Method names are the OpenAPI `operationId`s; `test/contract.test.ts` keeps them in step.

| Method | Endpoint | Success |
|---|---|---|
| `createMandate(body)` | `POST /v1/mandates` | 201 |
| `revokeMandate(id)` | `POST /v1/mandates/{id}/revoke` | 200 (repeatable) |
| `createInstruction(body)` | `POST /v1/instructions` | 201 new, 200 identical repeat |
| `amendInstruction(id, body)` | `POST /v1/instructions/{id}/revisions` | 201 new, 200 exact retry |
| `evaluateAction(body)` | `POST /v1/actions/evaluate` | 201 issued, 200 existing permit or DENY |
| `simulateAction(body)` | `POST /v1/actions/simulate` | 200 |
| `admitOperation(body)` | `POST /v1/operations` | 201 committed, 200 stored result |
| `getOperation(id)` | `GET /v1/operations/{id}` | 200 |
| `reconcileOperation(id)` | `POST /v1/operations/{id}/reconcile` | 200 (202 reserved for R1) |
| `getEvidence(id)` | `GET /v1/operations/{id}/evidence` | 200 |
| `getAdapterAssurance(id)` | `GET /v1/adapters/{id}/assurance` | 200 |

R1 (dispatch to a real destination, repairs and recovery):

| Method | Endpoint | Success |
|---|---|---|
| `dispatchOperation(body)` | `POST /v1/dispatches` | 201 recorded (PENDING), 200 identical repeat |
| `getDispatch(opRef)` | `GET /v1/dispatches/{op_ref}` | 200 |
| `settleDispatch(opRef)` | `POST /v1/dispatches/{op_ref}/settle` | 200 (repeatable) |
| `recordHeldFinding(opRef, body)` | `POST /v1/dispatches/{op_ref}/findings` | 201 |
| `prepareRepair(opRef, body)` | `POST /v1/dispatches/{op_ref}/repairs` | 201 |
| `reviewRepair(opRef, repairId, body)` | `POST /v1/dispatches/{op_ref}/repairs/{repair_id}/review` | 200 |
| `confirmRepair(opRef, repairId)` | `POST /v1/dispatches/{op_ref}/repairs/{repair_id}/confirm` | 200 (a repeat is 409) |
| `confirmAnchorRepair(opRef, body)` | `POST /v1/dispatches/{op_ref}/anchor-repair` | 200 |
| `getWriteState()` | `GET /v1/recovery/write-state` | 200 |
| `checkRestore()` | `POST /v1/recovery/check` | 200 |
| `reconcileRestore()` | `POST /v1/recovery/reconcile` | 200 (repeatable) |
| `resumeWrites()` | `POST /v1/recovery/resume` | 200 (`passed` false when the gate fails) |

Every method returns `ApiResponse<T>`: `{ status, created, requestId, attempts, data }`.
`created` is true exactly for HTTP 201.

## Errors
- `SteelmatterApiError`: the server's error envelope, only when the status is one the OpenAPI document lists for that operation, `error_code` is a known `ErrorCode`, and the status is the one that code is sent with (`ERROR_STATUS`, from `core/httpapi/codes.go`). `code` (closed `ErrorCode` union), `status`, `requestId`, `retryable` (429/503), `operationId` (look the operation up), `retryAfterMs` (429).
- `SteelmatterTransportError`: no response (connection failure or `timedOut`).
- `SteelmatterResponseError`: a reply outside the contract: non-JSON, a status the operation does not document (for example 409 from `getOperation`, or 418 or 502 from anything), an unknown `error_code`, a status/code mismatch (for example 404 + `RATE_LIMITED`), a redirect, or a body over 16 MiB. It is never retried.

## Timeouts and retries
- Per-attempt timeout `timeoutMs`, default 65 s (beyond the server's 60 s request deadline, so the client does not give up while the server may still commit). Pass `signal` to cancel.
- Retries (`retry: { maxAttempts: 3, baseDelayMs: 100, maxDelayMs: 2000, maxRetryAfterMs: 30000 }`, or `retry: false`): exponential backoff with jitter; 429 waits for `Retry-After`.
- What is retried follows document 03. Every endpoint except mandate creation is safe to repeat (identical retries return the existing result with the same identities), so 429, 503 and transport failures are retried. `createMandate` has no idempotency key and is retried only after 429, which the server returns before reading the body; after a 503 or a lost connection, check before creating another.
- Redirects are refused (a `SteelmatterResponseError`, not retried) so the bearer token only reaches the configured origin.
- If a custom `fetch` returns a body whose `cancel()` throws, rejects or never settles, an oversized response is still a `SteelmatterResponseError` and is not retried.
- Responses are capped at 16 MiB. The body is streamed and the download is cancelled as soon as the cap is passed, so a server that streams without (or with a false) `Content-Length` cannot make the client buffer an unbounded body. An oversized response is a `SteelmatterResponseError` and is not retried.

## Evidence
`getEvidence` returns records exactly as signed, without trust anchors. Verify them offline with
`@irontrex/steelmatter-verifier` and your own trust store; do not treat the `evidence` or `links` fields as proof.

## Helpers
`isPermitIssued` / `isDenied` (narrow `evaluateAction` results), `amountMinor(lines)` (checked integer total),
`formatINR(paise)` (`100000000` → `INR 10,00,000`).

## Tests
```bash
npm test              # unit, contract, type and installed-package tests (no server)
npm run typecheck     # strict tsc over src, tests and examples
```
The live test (`test/live/integration.live.ts`) drives all 11 operations against the real service and
PostgreSQL; it runs from Go: `go test -tags integration,failpoints -run '^TestSDK' ./tests/integration`.
