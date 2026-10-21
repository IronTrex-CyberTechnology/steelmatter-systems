import type { ApiResponse, EvaluateApiResponse } from './client.ts';
import type { Denied, EvaluateResponse, Line, PermitIssued } from './types.ts';

const MAX_SAFE = Number.MAX_SAFE_INTEGER; // 2^53-1, the API's integer bound

/** True when evaluation issued (or returned the existing) permit. Narrows `response.data` too. */
export function isPermitIssued(r: EvaluateApiResponse | ApiResponse<EvaluateResponse>): r is ApiResponse<PermitIssued>;
export function isPermitIssued(r: EvaluateResponse): r is PermitIssued;
export function isPermitIssued(r: EvaluateResponse | ApiResponse<EvaluateResponse>): boolean {
  return ('data' in r ? r.data : r).decision === 'PERMIT_ISSUED';
}

/** True for a policy denial (HTTP 200 with a reason). Narrows `response.data` too. */
export function isDenied(r: EvaluateApiResponse | ApiResponse<EvaluateResponse>): r is ApiResponse<Denied>;
export function isDenied(r: EvaluateResponse): r is Denied;
export function isDenied(r: EvaluateResponse | ApiResponse<EvaluateResponse>): boolean {
  return ('data' in r ? r.data : r).decision === 'DENY';
}

/**
 * The amount the server will compute for these lines: the sum of
 * quantity * unit_price_minor with checked arithmetic. Useful to check a
 * proposal against an allowance before submitting it; the server remains the
 * authority. Throws RangeError on a non-integer or overflow beyond 2^53-1.
 */
export function amountMinor(lines: readonly Line[]): number {
  let total = 0n;
  for (const l of lines) {
    if (!Number.isSafeInteger(l.quantity) || l.quantity < 1) throw new RangeError(`${l.item_id}: quantity must be an integer in [1, 2^53-1]`);
    if (!Number.isSafeInteger(l.unit_price_minor) || l.unit_price_minor < 0) throw new RangeError(`${l.item_id}: unit_price_minor must be an integer in [0, 2^53-1]`);
    total += BigInt(l.quantity) * BigInt(l.unit_price_minor);
    if (total > BigInt(MAX_SAFE)) throw new RangeError('amount exceeds 2^53-1');
  }
  return Number(total);
}

/**
 * Formats integer paise as Indian-grouped rupees, e.g. 100000000 → "INR 10,00,000".
 * Paise are shown only when non-zero ("INR 1,234.50").
 */
export function formatINR(paise: number): string {
  if (!Number.isSafeInteger(paise)) throw new RangeError('paise must be a safe integer');
  const neg = paise < 0;
  const abs = BigInt(Math.abs(paise));
  const rupees = (abs / 100n).toString();
  const rest = abs % 100n;
  const last3 = rupees.slice(-3);
  const head = rupees.slice(0, -3).replace(/\B(?=(\d{2})+(?!\d))/g, ',');
  const grouped = head ? `${head},${last3}` : last3;
  return `${neg ? '-' : ''}INR ${grouped}${rest === 0n ? '' : '.' + rest.toString().padStart(2, '0')}`;
}
