// Shapes of the embedded demo data (see build.mjs demoData()).
export interface ReferenceStep { name: string; ok: boolean }

export interface ReferenceScenario {
  scenario: string;
  case: string;
  test_ids: string[];
  fixtures: string[];
  status: string;
  ledger: string;
  steps: ReferenceStep[];
}

export interface Reference {
  schema: string;
  simulation: boolean;
  synthetic: boolean;
  note: string;
  generated_at_commit: string;
  generated_by: string;
  fixtures: Record<string, string>;
  scenarios: ReferenceScenario[];
}

export interface EvidenceCase {
  name: string;
  bundle: Record<string, unknown> & { records: Record<string, string>; evidence?: unknown; incomplete?: unknown };
  expect: Record<string, unknown>;
}

export interface EvidenceFixtures {
  fixture_set: string;
  note: string;
  trust_store: unknown;
  usable_at: string;
  cases: EvidenceCase[];
}

export interface Pinned<T> { file: string; sha256: string; data: T }

export interface DemoData {
  reference: Pinned<Reference>;
  evidence: Pinned<EvidenceFixtures>;
  build: { commit: string; dirty: boolean };
}
