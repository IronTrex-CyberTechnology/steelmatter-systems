// Evidence bundle verification. The report keeps every dimension separate:
// per-record integrity, issuer trust, payload/profile support, time-window
// usability, cross-record linkage and the observer's comparison. There is
// deliberately no single "valid" or "safe" verdict (document 03).
import { ContractError, type Code } from './errors.ts';
import { verify, type SignedKind } from './jws.ts';
import { validate, parseTimestamp } from './payloads.ts';
import { canonicalBytes, digest } from './canonical.ts';
import type { TrustStore } from './trust.ts';
import type { JsonValue } from './strict-json.ts';

export type Integrity = 'VALID' | 'INVALID' | 'UNVERIFIED' | 'MISSING';
export type Issuer = 'TRUSTED' | 'UNTRUSTED' | 'UNKNOWN';
export type Profile = 'SUPPORTED' | 'UNSUPPORTED' | 'UNKNOWN';

export interface RecordReport {
  integrity: Integrity;
  issuer: Issuer;
  profile: Profile;
  payload: 'VALID' | 'INVALID' | 'UNCHECKED';
  kid?: string;
  digest?: string;
  code?: Code;
  detail?: string;
  /** Key state at evaluation time: RETIRED keys still verify historical records. */
  key_status?: 'CURRENT' | 'RETIRED';
  /** Whether the record's own timestamp precedes its key's retirement (null if the key has none). */
  signed_within_key_validity?: boolean | null;
}

export interface LinkCheck {
  name: string;
  status: 'OK' | 'MISMATCH' | 'MISSING';
  detail?: string;
}

/**
 * Report format version. '2' (ADR-003) adds `chronology` and the five `time:*`
 * checks, which also appear in `linkage` so consumers of version '1' that gate
 * on linkage still fail closed. Nothing was removed or renamed.
 */
export const REPORT_VERSION = '2';

export interface EvidenceReport {
  report_version: typeof REPORT_VERSION;
  operation_id: string | null;
  evaluated_at: string;
  records: Record<SignedKind, RecordReport>;
  action: { payload: 'VALID' | 'INVALID' | 'MISSING'; digest?: string; code?: Code };
  usability: {
    permit: 'USABLE' | 'EXPIRED' | 'NOT_YET_VALID' | 'UNKNOWN';
    mandate: 'WITHIN_WINDOW' | 'EXPIRED' | 'NOT_YET_VALID' | 'UNKNOWN';
    note: string;
  };
  linkage: { status: 'COMPLETE' | 'INCOMPLETE' | 'BROKEN'; checks: LinkCheck[] };
  /**
   * Cross-record timestamp consistency (ADR-003), reported on its own. Uses
   * only the records' signed times, never the evaluation time, so authentic
   * historical evidence stays CONSISTENT after its permit expires.
   */
  chronology: { status: 'CONSISTENT' | 'INCONSISTENT' | 'INCOMPLETE'; checks: LinkCheck[] };
  observation: {
    status: 'MATCH' | 'MISMATCH' | 'INSUFFICIENT_EVIDENCE' | 'NOT_OBSERVED' | 'UNVERIFIED';
    observed_matches_action: boolean | null;
  };
  service_claims: { evidence: unknown; incomplete: unknown; note: string };
}

const KINDS: SignedKind[] = ['mandate', 'permit', 'execution', 'observation'];
const UNSUPPORTED_CODES = new Set<Code>(['JWS_ALG_UNSUPPORTED', 'JWS_HEADER_UNSUPPORTED', 'JWS_TYPE_MISMATCH', 'UNSUPPORTED_VERSION', 'UNSUPPORTED_PROFILE']);
const JWS_STRUCTURE = new Set<Code>(['JWS_MALFORMED', 'JWS_ALG_UNSUPPORTED', 'JWS_HEADER_UNSUPPORTED', 'JWS_TYPE_MISMATCH']);

type Payload = Record<string, any>;

const RECORD_TIME: Record<SignedKind, string> = { mandate: 'issued_at', permit: 'issued_at', execution: 'committed_at', observation: 'observed_at' };

async function checkRecord(kind: SignedKind, jws: unknown, trust: TrustStore, now: number): Promise<{ report: RecordReport; payload?: Payload }> {
  if (typeof jws !== 'string' || jws === '') {
    return { report: { integrity: 'MISSING', issuer: 'UNKNOWN', profile: 'UNKNOWN', payload: 'UNCHECKED' } };
  }
  try {
    const v = await verify(jws, kind, trust);
    const report: RecordReport = { integrity: 'VALID', issuer: 'TRUSTED', profile: 'SUPPORTED', payload: 'VALID', kid: v.kid, digest: v.digest };
    const notAfter = v.key.not_after === undefined ? undefined : Date.parse(v.key.not_after) / 1000;
    report.key_status = notAfter !== undefined && now >= notAfter ? 'RETIRED' : 'CURRENT';
    if (notAfter === undefined) {
      report.signed_within_key_validity = null;
    } else {
      const at = parseTimestamp(String((v.value as Payload)[RECORD_TIME[kind]]));
      report.signed_within_key_validity = at !== null && at < notAfter;
      if (!report.signed_within_key_validity) {
        // Claims to have been signed after its key was retired: not trustworthy evidence.
        return { report: { ...report, issuer: 'UNTRUSTED', code: 'JWS_KEY_UNTRUSTED', detail: 'record time is at or after the signing key\'s retirement' } };
      }
    }
    return { report, payload: v.value as Payload };
  } catch (e) {
    if (!(e instanceof ContractError)) throw e;
    const code = e.code;
    const profile: Profile = UNSUPPORTED_CODES.has(code) ? 'UNSUPPORTED' : 'UNKNOWN';
    if (code === 'JWS_KEY_UNTRUSTED') {
      return { report: { integrity: 'UNVERIFIED', issuer: 'UNTRUSTED', profile, payload: 'UNCHECKED', code, detail: e.message } };
    }
    if (JWS_STRUCTURE.has(code)) {
      return { report: { integrity: 'INVALID', issuer: 'UNKNOWN', profile, payload: 'UNCHECKED', code, detail: e.message } };
    }
    if (code === 'JWS_SIGNATURE_INVALID') {
      return { report: { integrity: 'INVALID', issuer: 'TRUSTED', profile: 'SUPPORTED', payload: 'UNCHECKED', code, detail: e.message } };
    }
    // The signature verified under a trusted key, but the payload breaks the contract.
    return { report: { integrity: 'VALID', issuer: 'TRUSTED', profile: profile === 'UNSUPPORTED' ? 'UNSUPPORTED' : 'SUPPORTED', payload: 'INVALID', code, detail: e.message } };
  }
}

/**
 * Verifies an evidence bundle (GET /v1/operations/{id}/evidence) against
 * caller-supplied trust anchors. `now` (epoch seconds) is only used for the
 * advisory usability window; a historical expired permit can still be
 * authentic evidence.
 */
export async function verifyBundle(bundle: JsonValue, trust: TrustStore, now: number = Math.floor(Date.now() / 1000)): Promise<EvidenceReport> {
  // Unknown envelope members are outside every verification dimension. Reject
  // them rather than silently returning the same report as a clean bundle.
  const closed = (value: unknown, keys: string[], path: string) => {
    if (value === undefined) return; // absent records/links retain incomplete-report handling
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
      throw new ContractError('SCHEMA_VIOLATION', path, 'evidence envelope member must be an object');
    }
    for (const key of Object.keys(value)) {
      if (!keys.includes(key)) {
        const pointer = `${path}/${key.replaceAll('~', '~0').replaceAll('/', '~1')}`;
        throw new ContractError('UNKNOWN_FIELD', pointer, 'unknown evidence envelope field');
      }
    }
  };
  closed(bundle, ['operation_id', 'evidence', 'records', 'action', 'links', 'incomplete'], '');
  const envelope = bundle as Record<string, JsonValue> | null;
  closed(envelope?.records, KINDS.map(k => `${k}_jws`), '/records');
  closed(envelope?.links, ['instruction_id', 'action_digest', 'mandate_digest', 'permit_digest', 'execution_digest', 'obligation_id'], '/links');
  const b: any = typeof bundle === 'object' && bundle !== null && !Array.isArray(bundle) ? bundle : {};
  const recs: any = typeof b.records === 'object' && b.records !== null ? b.records : {};
  const payloads: Partial<Record<SignedKind, Payload>> = {};
  const records = {} as Record<SignedKind, RecordReport>;
  for (const k of KINDS) {
    const r = await checkRecord(k, recs[`${k}_jws`], trust, now);
    records[k] = r.report;
    if (r.payload) payloads[k] = r.payload;
  }

  // The action is digested, not signed: validate and recompute.
  const report: EvidenceReport = {
    report_version: REPORT_VERSION,
    operation_id: typeof b.operation_id === 'string' ? b.operation_id : null,
    evaluated_at: new Date(now * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z'),
    records,
    action: { payload: 'MISSING' },
    usability: { permit: 'UNKNOWN', mandate: 'UNKNOWN', note: 'Time window only. Consumption, revocation and amendment state are known only to the service.' },
    linkage: { status: 'COMPLETE', checks: [] },
    chronology: { status: 'CONSISTENT', checks: [] },
    observation: { status: 'NOT_OBSERVED', observed_matches_action: null },
    service_claims: { evidence: b.evidence ?? null, incomplete: b.incomplete ?? null, note: 'Reported by the service; not used for any result above.' },
  };
  let action: Payload | undefined;
  let actionDigest: string | undefined;
  if (b.action !== undefined) {
    try {
      const bytes = canonicalBytes(b.action);
      action = validate('action', bytes) as Payload;
      actionDigest = digest('action', bytes);
      report.action = { payload: 'VALID', digest: actionDigest };
    } catch (e) {
      report.action = { payload: 'INVALID', code: e instanceof ContractError ? e.code : undefined };
    }
  }

  const { mandate, permit, execution, observation } = payloads;
  const checks = report.linkage.checks;
  const check = (name: string, parts: unknown[], equal: () => boolean, detail?: string) => {
    if (parts.some((p) => p === undefined || p === null)) checks.push({ name, status: 'MISSING' });
    else checks.push(equal() ? { name, status: 'OK' } : { name, status: 'MISMATCH', detail });
  };
  const same = (...vals: unknown[]) => () => vals.every((v) => v === vals[0]);
  const mandateDigest = records.mandate.digest;
  const permitDigest = records.permit.digest;
  const executionDigest = records.execution.digest;

  check('action_digest:permit', [actionDigest, permit], () => permit!.action_digest === actionDigest);
  check('action_digest:execution', [actionDigest, execution], () => execution!.action_digest === actionDigest);
  check('action_digest:observation', [actionDigest, observation], () => observation!.expected_action_digest === actionDigest);
  check('mandate_digest:permit', [mandateDigest, permit], () => permit!.mandate_digest === mandateDigest);
  check('mandate_identity:permit', [mandate, permit], () =>
    permit!.mandate_id === mandate!.mandate_id && permit!.mandate_revision === mandate!.revision && permit!.control_epoch === mandate!.control_epoch);
  check('permit_digest:execution', [permitDigest, execution], () => execution!.permit_digest === permitDigest);
  check('execution_digest:observation', [executionDigest, observation], () => observation!.execution_digest === executionDigest);
  check('operation_id', [report.operation_id, permit, execution, observation],
    same(report.operation_id, permit?.operation_id, execution?.operation_id, observation?.operation_id));
  check('instruction_id', [action, permit, execution, observation],
    same(action?.instruction_id, permit?.instruction_id, execution?.instruction_id, observation?.instruction_id));
  check('tenant_id', [mandate, action, permit, execution, observation],
    same(mandate?.tenant_id, action?.tenant_id, permit?.tenant_id, execution?.tenant_id, observation?.tenant_id));
  check('actor_and_audience', [mandate, action, permit], () =>
    permit!.actor_id === action!.actor_id && permit!.audience === action!.audience && mandate!.audience === action!.audience &&
    mandate!.actor_ids.includes(action!.actor_id));
  check('within_mandate_scope', [mandate, action], () =>
    mandate!.supplier_ids.includes(action!.supplier_id) && mandate!.delivery_ids.includes(action!.delivery_id) && action!.amount_minor <= mandate!.limit_minor);
  check('action_revision', [action, permit], () => permit!.action_revision === action!.action_revision);
  check('amount_minor', [action, permit, execution], same(action?.amount_minor, permit?.amount_minor, execution?.amount_minor));
  check('target_order_id', [execution, observation], () => execution!.target_order_id === observation!.target_order_id);
  check('obligation_id', [execution], () => typeof execution!.obligation_id === 'string' && (b.links?.obligation_id === undefined || b.links.obligation_id === execution!.obligation_id));
  // Cross-record timestamps (ADR-003): whole UTC seconds from one clock,
  // exclusive expiry. Same rules and names as Go contract.TemporalLinkage.
  // An unparsable time (only reachable by bypassing validation) never holds.
  const t = (v: unknown) => (typeof v === 'string' ? parseTimestamp(v) : null);
  const le = (a: unknown, b: unknown) => { const x = t(a); const y = t(b); return x !== null && y !== null && x <= y; };
  const lt = (a: unknown, b: unknown) => { const x = t(a); const y = t(b); return x !== null && y !== null && x < y; };
  check('time:permit_issued_within_mandate', [mandate, permit], () => le(mandate!.issued_at, permit!.issued_at) && lt(permit!.issued_at, mandate!.expires_at),
    'permit issued outside its mandate window');
  check('time:permit_expiry_within_mandate', [mandate, permit], () => le(permit!.expires_at, mandate!.expires_at), 'permit outlives its mandate');
  check('time:execution_within_permit', [permit, execution], () => le(permit!.issued_at, execution!.committed_at) && lt(execution!.committed_at, permit!.expires_at),
    'execution committed outside the permit window');
  check('time:execution_within_mandate', [mandate, execution], () => le(mandate!.issued_at, execution!.committed_at) && lt(execution!.committed_at, mandate!.expires_at),
    'execution committed outside the mandate window');
  check('time:observation_after_execution', [execution, observation], () => le(execution!.committed_at, observation!.observed_at),
    'observation recorded before the execution it observes');
  // The service's link claims must agree with what was recomputed.
  const claims = b.links ?? {};
  const claimMismatch = [
    ['instruction_id', action?.instruction_id],
    ['action_digest', actionDigest], ['mandate_digest', mandateDigest], ['permit_digest', permitDigest], ['execution_digest', executionDigest],
  ].filter(([k, v]) => claims[k as string] !== undefined && v !== undefined && claims[k as string] !== v).map(([k]) => k);
  checks.push(claimMismatch.length === 0 ? { name: 'service_link_claims', status: 'OK' } : { name: 'service_link_claims', status: 'MISMATCH', detail: claimMismatch.join(',') });

  report.linkage.status = checks.some((c) => c.status === 'MISMATCH') ? 'BROKEN' : checks.some((c) => c.status === 'MISSING') ? 'INCOMPLETE' : 'COMPLETE';
  const time = checks.filter((c) => c.name.startsWith('time:'));
  report.chronology = {
    status: time.some((c) => c.status === 'MISMATCH') ? 'INCONSISTENT' : time.some((c) => c.status === 'MISSING') ? 'INCOMPLETE' : 'CONSISTENT',
    checks: time.map((c) => ({ ...c })),
  };

  if (permit) {
    const i = parseTimestamp(permit.issued_at)!;
    const x = parseTimestamp(permit.expires_at)!;
    report.usability.permit = now < i ? 'NOT_YET_VALID' : now >= x ? 'EXPIRED' : 'USABLE';
  }
  if (mandate) {
    const i = parseTimestamp(mandate.issued_at)!;
    const x = parseTimestamp(mandate.expires_at)!;
    report.usability.mandate = now < i ? 'NOT_YET_VALID' : now >= x ? 'EXPIRED' : 'WITHIN_WINDOW';
  }
  if (observation) {
    report.observation.status = observation.status;
    report.observation.observed_matches_action =
      actionDigest === undefined || observation.observed_action_digest === null ? null : observation.observed_action_digest === actionDigest;
  } else if (records.observation.integrity !== 'MISSING') {
    report.observation.status = 'UNVERIFIED';
  }
  return report;
}
