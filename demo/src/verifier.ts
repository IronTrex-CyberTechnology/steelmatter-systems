// The standalone verifier, re-exported unchanged for the browser bundle.
export {
  verifyBundle, loadTrustStore, verify, validate, canonicalizeRaw, digest, ContractError,
} from '../../packages/verifier/src/index.ts';
export type { EvidenceReport, RecordReport, LinkCheck, TrustStore, Kind, SignedKind } from '../../packages/verifier/src/index.ts';
