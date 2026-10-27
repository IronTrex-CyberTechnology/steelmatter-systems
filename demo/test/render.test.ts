// T43 (browser): every screen shows what the recording actually contains,
// with the exact case, test IDs, fixtures, commit and simulated status, and a
// changed recording is detected. Also the document 13 rules that can be
// checked without a browser: persistent labels, text for every state, reason
// codes for denials, client view separate from database view.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as node from '../../packages/verifier/src/index.ts';
import { CARDS, STEP_NOTES, coverageProblems } from '../src/notes.ts';
import { esc, evidenceView, inr, overview, page, parseLedger, parseRoute, quickstart, scenarioView, type Route } from '../src/render.ts';
import { data } from './helpers.ts';

const ref = data.reference.data;
// Decode &amp; last: decoding it first would turn an escaped literal "&quot;"
// (rendered as "&amp;quot;") into a quote character (CodeQL js/double-escaping).
const textOf = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/\s+/g, ' ');

test('textOf decodes each entity once, keeping literal &quot; and &#39; text', () => {
  assert.equal(textOf('&amp;quot;'), '&quot;');
  assert.equal(textOf('&amp;#39;'), '&#39;');
  assert.equal(textOf('&amp;amp;'), '&amp;');
  assert.equal(textOf('&quot;a&quot; &#39;b&#39; c &amp; d'), `"a" 'b' c & d`);
  for (const s of ['&quot;', '&#39;', 'say &quot;hi&quot; or &#39;bye&#39;', `"quoted" 'single' & amp`, 'A&amp;B']) {
    assert.equal(textOf(esc(s)), s, `round trip of ${JSON.stringify(s)}`);
  }
});

test('T43: every recorded step has a note, every note a recorded step', () => {
  assert.deepEqual(coverageProblems(ref.scenarios), []);
});

test('T43: a changed recording is detected', () => {
  const changed = structuredClone(ref.scenarios);
  changed[0].steps[1].name = 'a step the page does not know';
  changed.push({ ...changed[1], scenario: 'unexpected-scenario' });
  const problems = coverageProblems(changed);
  assert.ok(problems.some((p) => p.includes('a step the page does not know')));
  assert.ok(problems.some((p) => p.includes('unexpected-scenario')));
});

test('T43: each scenario screen shows its case, every test ID, its fixtures, the recorded commit and simulated status', () => {
  const commit = ref.generated_at_commit.replace(/-dirty$/, '').slice(0, 7);
  for (const card of CARDS.filter((c) => c.scenario)) {
    const s = ref.scenarios.find((x) => x.scenario === card.scenario)!;
    const t = textOf(scenarioView(data, card.id, s.steps.length));
    assert.ok(t.includes(s.case), `${card.id} case`);
    for (const id of s.test_ids) assert.ok(t.includes(id), `${card.id} ${id}`);
    for (const f of s.fixtures) assert.ok(t.includes(f), `${card.id} ${f}`);
    assert.ok(t.includes(commit), `${card.id} commit`);
    assert.ok(t.includes('Simulated, recorded playback'), `${card.id} simulated status`);
    assert.ok(t.includes(`Recorded result: ${s.status}`), `${card.id} status`);
    for (const [i, step] of s.steps.entries()) assert.ok(t.includes(`${i + 1}. ${step.name}`), `${card.id} step ${step.name}`);
  }
  if (ref.generated_at_commit.endsWith('-dirty')) {
    assert.ok(textOf(overview(data)).length > 0);
    assert.ok(textOf(page(data, { screen: 'overview' }, { shown: 0 })).includes('working tree with uncommitted changes'));
  }
});

test('the overview has the five document 13 cards and says plainly when a case has no recording', () => {
  const t = textOf(overview(data));
  for (const id of ['T01', 'T04', 'T09', 'T11', 'T13']) assert.ok(CARDS.some((c) => c.id === id) && t.includes(id), id);
  for (const card of CARDS) {
    if (card.scenario) assert.ok(t.includes(`Play ${card.id}`), card.id);
    else assert.ok(t.includes('Not recorded yet') && t.includes('cannot be played'), card.id);
  }
  assert.ok(t.includes('Reset to the synthetic starting state'));
});

test('every screen carries the Simulation, Synthetic data and recorded-playback labels', () => {
  const routes: Route[] = [{ screen: 'overview' }, { screen: 'scenario', card: 'T01' }, { screen: 'evidence', case: 'complete' }, { screen: 'quickstart' }];
  for (const r of routes) {
    const t = textOf(page(data, r, { shown: 0 }));
    for (const label of ['Simulation', 'Synthetic data', 'Recorded playback, not a live run']) assert.ok(t.includes(label), `${r.screen}: ${label}`);
  }
});

test('playback reveals steps one at a time and states progress in text', () => {
  const s = ref.scenarios.find((x) => x.scenario === 'flagship')!;
  const start = textOf(scenarioView(data, 'T01', 0));
  assert.ok(start.includes(`Working: step 0 of ${s.steps.length}`));
  assert.ok(!start.includes(`1. ${s.steps[0].name}`));
  assert.ok(start.includes('The recorded end state appears when playback reaches the end'));
  const two = textOf(scenarioView(data, 'T01', 2));
  assert.ok(two.includes(`2. ${s.steps[1].name}`) && !two.includes(`3. ${s.steps[2].name}`));
  assert.ok(textOf(scenarioView(data, 'T01', s.steps.length)).includes(`Finished: ${s.steps.length} of ${s.steps.length} steps`));
});

test('denials show their reason code in text, not only colour', () => {
  for (const s of ref.scenarios) {
    const card = CARDS.find((c) => c.scenario === s.scenario)!;
    const t = textOf(scenarioView(data, card.id, s.steps.length));
    for (const step of s.steps) {
      const n = STEP_NOTES[s.scenario][step.name];
      if (n.outcome === 'denied') assert.ok(n.reason && t.includes(`Denied: ${n.reason}`), `${s.scenario}: ${step.name}`);
    }
  }
});

test('the end ledger shown is the recorded one, in INR with minor units', () => {
  for (const s of ref.scenarios) {
    const l = parseLedger(s.ledger);
    assert.ok(l, `${s.scenario} ledger parses`);
    const card = CARDS.find((c) => c.scenario === s.scenario)!;
    const html = scenarioView(data, card.id, s.steps.length);
    assert.ok(textOf(html).includes(`Available ${inr(l!.available)}`), s.scenario);
    assert.ok(html.includes(`<code>${l!.committed}</code>`), `${s.scenario} minor units`);
  }
  assert.equal(inr(100_000_000), 'INR 10,00,000');
});

test('the lost-response run keeps the client view separate from the database view', () => {
  const s = ref.scenarios.find((x) => x.scenario === 'lost-response')!;
  const t = textOf(scenarioView(data, 'T13', s.steps.length));
  assert.ok(t.includes('Client view') && t.includes('Database view') && t.includes('Client unsure'));
});

test('controls are real buttons and every status chip has text', () => {
  const html = page(data, { screen: 'scenario', card: 'T01' }, { shown: 3 });
  for (const a of ['restart', 'prev', 'next', 'play']) assert.match(html, new RegExp(`<button type="button" data-action="${a}"`));
  for (const m of html.matchAll(/<span class="chip [^"]+"><span aria-hidden="true">[^<]*<\/span>([^<]*)<\/span>/g)) assert.ok(m[1].trim().length > 0, 'chip without text');
});

test('evidence screen: working, error with retry, and verified results with fixture expectations matched', async () => {
  assert.ok(textOf(evidenceView(data, 'complete', { status: 'loading' })).includes('Working: checking signatures'));
  const err = evidenceView(data, 'complete', { status: 'error', message: 'synthetic failure' });
  assert.ok(err.includes('role="alert"') && err.includes('data-action="retry-evidence"') && err.includes('synthetic failure'));
  const fx = data.evidence.data;
  const trust = await node.loadTrustStore(fx.trust_store as never);
  const at = Math.floor(Date.parse(fx.usable_at) / 1000);
  const want: Record<string, string> = {
    complete: 'Evidence complete and consistent', 'broken-permit-link': 'Evidence mismatch: broken linkage',
    'observer-mismatch': 'Evidence mismatch: observed order differs', 'incomplete-pending': 'Evidence incomplete',
  };
  for (const c of fx.cases) {
    const result = { atFixture: await node.verifyBundle(c.bundle as never, trust, at), now: await node.verifyBundle(c.bundle as never, trust) };
    const t = textOf(evidenceView(data, c.name, { status: 'ok', result }));
    assert.ok(t.includes(want[c.name]), c.name);
    assert.ok(t.includes('Matches') && !t.includes('Differs'), `${c.name} expectations`);
    for (const label of ['Signature integrity', 'Trusted issuer', 'Execution state', 'Observation comparison']) assert.ok(t.includes(label), `${c.name} ${label}`);
    assert.ok(t.includes('Download this synthetic evidence'));
  }
});

test('routes resolve, and unknown routes fall back to the overview', () => {
  assert.deepEqual(parseRoute('#/scenario/T13', data), { screen: 'scenario', card: 'T13' });
  assert.deepEqual(parseRoute('#/scenario/T99', data), { screen: 'overview' });
  assert.deepEqual(parseRoute('#/evidence/unknown', data), { screen: 'evidence', case: data.evidence.data.cases[0].name });
  assert.ok(textOf(quickstart(data)).includes('scripts/demo'));
});

test('evidence screen states an impossible timeline explicitly (ADR-003)', async () => {
  const temporal = JSON.parse((await import('node:fs')).readFileSync(new URL('../../fixtures/vectors/evidence-temporal-v1.json', import.meta.url), 'utf8'));
  const trust = await node.loadTrustStore(temporal.trust_store);
  const c = temporal.cases.find((x: { name: string }) => x.name === 'observation-before-execution');
  const result = { atFixture: await node.verifyBundle(c.bundle, trust), now: await node.verifyBundle(c.bundle, trust) };
  const view = { ...data, evidence: { ...data.evidence, data: { ...data.evidence.data, trust_store: temporal.trust_store, cases: [{ ...c, expect: {} }] } } };
  const t = textOf(evidenceView(view as never, c.name, { status: 'ok', result }));
  assert.ok(t.includes('Evidence mismatch: impossible timeline'));
  assert.ok(t.includes('Chronology') && t.includes('INCONSISTENT') && t.includes('time:observation_after_execution'));
  const ok = temporal.cases.find((x: { name: string }) => x.name === 'observation-same-second-as-execution');
  const okResult = { atFixture: await node.verifyBundle(ok.bundle, trust), now: await node.verifyBundle(ok.bundle, trust) };
  const okView = { ...view, evidence: { ...view.evidence, data: { ...view.evidence.data, cases: [{ ...ok, expect: {} }] } } };
  const ot = textOf(evidenceView(okView as never, ok.name, { status: 'ok', result: okResult }));
  assert.ok(ot.includes('CONSISTENT') && !ot.includes('impossible timeline'));
});
