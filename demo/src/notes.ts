// Plain-language notes for each recorded step, written from the scenario
// source (examples/procurement/scenarios.go). The recorded run itself stores
// only the step name and whether it passed; these notes explain it. The demo
// tests require a note for every recorded step and no note without a step,
// so a changed recording cannot drift away from what the page says.

export type Phase = 'Setup' | 'Proposal' | 'Reservation' | 'Admission' | 'Observation';
export const PHASES: Phase[] = ['Setup', 'Proposal', 'Reservation', 'Admission', 'Observation'];

/** What the step demonstrates; every outcome is shown with a text label, never colour alone. */
export type Outcome = 'done' | 'denied' | 'committed' | 'client-unknown' | 'checked';

export interface StepNote {
  phase: Phase;
  actor: string;
  detail: string;
  outcome: Outcome;
  /** Stable reason code the scenario expects (from the scenario source). */
  reason?: string;
  /** A material change the step makes or attempts. */
  change?: string;
  /** For the lost-response scenario: whose view the step shows. */
  lane?: 'client' | 'database';
  /** Acceptance cases this step demonstrates beyond the scenario's own. */
  cases?: string[];
}

export const START_LIMIT_MINOR = 100_000_000; // INR 10,00,000 in paise, as every scenario starts

export const STEP_NOTES: Record<string, Record<string, StepNote>> = {
  flagship: {
    'owner grants mandate': { phase: 'Setup', actor: 'owner-a', outcome: 'done', detail: 'The owner signs a mandate: INR 10,00,000 for agent-a and agent-b, supplier-a only.' },
    'trusted workflows create two instructions': { phase: 'Proposal', actor: 'trusted workflows', outcome: 'done', detail: 'Two trusted workflows each create one instruction, assigned to agent-a and agent-b. The agents cannot create instructions themselves.' },
    'concurrent INR 6,00,000 requests': { phase: 'Reservation', actor: 'agent-a and agent-b', outcome: 'denied', reason: 'CAPACITY_EXHAUSTED', detail: 'Both agents ask for INR 6,00,000 at the same moment. Exactly one permit is issued; the other request is denied because only INR 4,00,000 would remain.' },
    'one reservation held': { phase: 'Reservation', actor: 'service', outcome: 'checked', detail: 'The ledger holds one live reservation of INR 6,00,000; the denial reserved nothing.' },
    'winner presents the exact action and permit': { phase: 'Admission', actor: 'winning agent', outcome: 'committed', detail: 'The winner presents the exact signed action with its permit. The order is created (HTTP 201).' },
    'committed, nothing reserved': { phase: 'Admission', actor: 'service', outcome: 'checked', detail: 'INR 6,00,000 moves from reserved to committed in the same transaction as the order.' },
    'retry returns the same order': { phase: 'Admission', actor: 'winning agent', outcome: 'checked', cases: ['T09'], detail: 'The same permit is presented again. The service returns the original order (HTTP 200) instead of creating another.' },
    'no second charge': { phase: 'Admission', actor: 'service', outcome: 'checked', cases: ['T09'], detail: 'The ledger still shows one order and INR 6,00,000 committed: the replay changed nothing.' },
    'observer reads the stored order back': { phase: 'Observation', actor: 'observer', outcome: 'checked', detail: 'The read-only observer reads the order from the target and finds it matches the authorised action (MATCH).' },
    'signed evidence exported': { phase: 'Observation', actor: 'service', outcome: 'checked', detail: 'Signed evidence for the operation is available for offline verification.' },
  },
  'stale-supplier': {
    'permit issued for supplier revision': { phase: 'Reservation', actor: 'agent-a', outcome: 'done', detail: 'A permit is issued that binds the current revision of the supplier-a record.' },
    'stale permit rejected': { phase: 'Admission', actor: 'agent-a', outcome: 'denied', reason: 'SUPPLIER_REVISION_STALE', change: 'The supplier-a record changed after the permit was issued (revision + 1).', detail: 'Admission is refused because the permit names an old supplier revision (HTTP 409).' },
    'old revision cannot be re-permitted': { phase: 'Reservation', actor: 'agent-a', outcome: 'denied', reason: 'SUPPLIER_REVISION_STALE', detail: 'Asking again for the same action revision is refused too; the old supplier data cannot be reused.' },
    'issuer re-authorises via amendment': { phase: 'Proposal', actor: 'issuer', outcome: 'done', change: 'Action revision 2 binds the new supplier revision.', detail: 'The issuer amends the instruction; the unused reservation is released.' },
    'new revision evaluated and committed': { phase: 'Admission', actor: 'agent-a', outcome: 'committed', detail: 'The new revision gets a new permit and commits.' },
    'one order at the current supplier revision': { phase: 'Admission', actor: 'service', outcome: 'checked', detail: 'Exactly one order exists, at the current supplier revision.' },
  },
  'lost-response': {
    'permit issued': { phase: 'Reservation', actor: 'agent-a', outcome: 'done', detail: 'A permit is issued for the operation.' },
    'response lost in transit': { phase: 'Admission', actor: 'agent-a (client view)', outcome: 'client-unknown', lane: 'client', detail: 'The admission request reaches the service, but the response is lost on the way back. The client does not know whether the order was placed.' },
    'database truth: committed once': { phase: 'Admission', actor: 'service (database view)', outcome: 'committed', lane: 'database', detail: 'The database shows the order committed exactly once, whatever the client believes.' },
    'client looks up the operation': { phase: 'Admission', actor: 'agent-a (client view)', outcome: 'checked', lane: 'client', detail: 'The client looks up the operation and learns it is COMMITTED.' },
    'retry returns the original result': { phase: 'Admission', actor: 'agent-a (client view)', outcome: 'checked', lane: 'client', detail: 'A retry with the same permit returns the original operation, not a new one.' },
    'no release, no second charge': { phase: 'Admission', actor: 'service (database view)', outcome: 'checked', lane: 'database', detail: 'Nothing was released and nothing was charged twice.' },
  },
  'agent-injection': {
    'permit issued for the authorised order': { phase: 'Reservation', actor: 'agent-a', outcome: 'done', detail: 'A permit is issued for 3 × item-a from supplier-a, INR 6,00,000. The agent then reads supplier text containing injected instructions (fixtures/demo/supplier-message.txt).' },
    'switch supplier with the issued permit': { phase: 'Admission', actor: 'agent-a', outcome: 'denied', reason: 'ACTION_CHANGED', change: 'Supplier switched to supplier-b.', detail: 'The permit does not cover a different supplier (HTTP 409).' },
    'inflate the order to INR 30,00,000': { phase: 'Admission', actor: 'agent-a', outcome: 'denied', reason: 'ACTION_CHANGED', change: 'Amount raised to INR 30,00,000.', detail: 'The permit does not cover a larger amount (HTTP 409).' },
    'raise its own allowance': { phase: 'Setup', actor: 'agent-a', outcome: 'denied', reason: 'FORBIDDEN', detail: 'An agent cannot create or widen a mandate (HTTP 403).' },
    'create a follow-up order itself': { phase: 'Proposal', actor: 'agent-a', outcome: 'denied', reason: 'FORBIDDEN', detail: 'An agent cannot create instructions (HTTP 403).' },
    'amend the instruction to supplier-b': { phase: 'Proposal', actor: 'agent-a', outcome: 'denied', reason: 'FORBIDDEN', change: 'Amendment to supplier-b.', detail: 'An agent cannot amend instructions (HTTP 403).' },
    'no unauthorised effect': { phase: 'Admission', actor: 'service', outcome: 'checked', detail: 'None of the injected attempts changed the ledger or created an order.' },
    'only the exact authorised order can commit': { phase: 'Admission', actor: 'agent-a', outcome: 'committed', detail: 'The original, exact action commits (HTTP 201).' },
    'one authorised order': { phase: 'Admission', actor: 'service', outcome: 'checked', detail: 'Exactly one order exists: the one the owner authorised.' },
  },
};

/** The scenario cards on the overview screen (document 13, screen A). */
export interface Card {
  id: string;
  title: string;
  /** Recorded scenario to play, or null when no recording exists yet. */
  scenario: string | null;
  summary: string;
  /** Steps to emphasise when the card shares a recording with another case. */
  focus?: string;
  /** Tests for a card without a recording. */
  tests?: string[];
  extra?: boolean;
}

export const CARDS: Card[] = [
  { id: 'T01', title: 'Shared allowance', scenario: 'flagship', summary: 'Two agents ask for INR 6,00,000 each from one INR 10,00,000 allowance at the same time. Only one can have it.' },
  { id: 'T04', title: 'Changed supplier', scenario: 'stale-supplier', summary: 'The supplier record changes after a permit is issued. The old permit stops working.' },
  { id: 'T09', title: 'Duplicate', scenario: 'flagship', focus: 'T09', summary: 'The same permit is presented twice. The second time returns the first order; nothing is charged again. Recorded within the shared-allowance run.' },
  { id: 'T11', title: 'Request-ID reroll', scenario: null, tests: ['TestT11IDReroll'], summary: 'The same source record is sent again with a new client request ID. The service must return the original instruction, not create a second one.' },
  { id: 'T13', title: 'Lost response', scenario: 'lost-response', summary: 'The order is placed but the reply is lost. The client is unsure; the database is not.' },
  { id: 'T32', title: 'Agent injection', scenario: 'agent-injection', extra: true, summary: 'Injected supplier text tries to make the agent change supplier, amount or its own powers.' },
];

/** Named synthetic participants per scenario (from the scenario source). */
export const PEOPLE: Record<string, { owner: string; agents: string[]; supplier: string }> = {
  flagship: { owner: 'owner-a', agents: ['agent-a', 'agent-b'], supplier: 'supplier-a' },
  'stale-supplier': { owner: 'owner-a', agents: ['agent-a'], supplier: 'supplier-a' },
  'lost-response': { owner: 'owner-a', agents: ['agent-a'], supplier: 'supplier-a' },
  'agent-injection': { owner: 'owner-a', agents: ['agent-a'], supplier: 'supplier-a' },
};

/** Short descriptions of the synthetic evidence chains in fixtures/vectors/evidence-v1.json. */
export const EVIDENCE_LABELS: Record<string, { title: string; summary: string }> = {
  complete: { title: 'Complete chain', summary: 'Mandate, permit, execution and observation, all signed and linked.' },
  'broken-permit-link': { title: 'Broken link', summary: 'The execution record points at a different permit.' },
  'observer-mismatch': { title: 'Observer mismatch', summary: 'The observer saw an order that differs from the authorised action.' },
  'incomplete-pending': { title: 'Incomplete', summary: 'Execution is not signed yet and nothing has been observed.' },
};

/** Differences between the recorded steps and these notes (empty when in sync). */
export function coverageProblems(scenarios: { scenario: string; steps: { name: string }[] }[]): string[] {
  const problems: string[] = [];
  const recorded = new Set(scenarios.map((s) => s.scenario));
  for (const s of scenarios) {
    const notes = STEP_NOTES[s.scenario];
    if (!notes) { problems.push(`no notes for recorded scenario ${s.scenario}`); continue; }
    if (!PEOPLE[s.scenario]) problems.push(`no participants for recorded scenario ${s.scenario}`);
    const names = new Set(s.steps.map((x) => x.name));
    for (const n of names) if (!notes[n]) problems.push(`${s.scenario}: recorded step "${n}" has no note`);
    for (const n of Object.keys(notes)) if (!names.has(n)) problems.push(`${s.scenario}: note "${n}" has no recorded step`);
  }
  for (const k of Object.keys(STEP_NOTES)) if (!recorded.has(k)) problems.push(`notes for ${k}, which is not recorded`);
  for (const c of CARDS) if (c.scenario && !recorded.has(c.scenario)) problems.push(`card ${c.id} plays ${c.scenario}, which is not recorded`);
  return problems;
}
