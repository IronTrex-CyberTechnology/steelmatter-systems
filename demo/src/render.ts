// Pure rendering: data in, HTML string out. No DOM access, so the demo tests
// can render every screen in Node and check what a viewer would see.
import type { DemoData, EvidenceCase, ReferenceScenario } from './types.ts';
import type { EvidenceReport } from './verifier.ts';
import { CARDS, EVIDENCE_LABELS, PEOPLE, PHASES, START_LIMIT_MINOR, STEP_NOTES, type Card, type Outcome, type StepNote } from './notes.ts';

export const REPO = '../..'; // links are relative to demo/dist/index.html inside a checkout

const ESC: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const esc = (v: unknown) => String(v ?? '').replace(/[&<>"']/g, (c) => ESC[c]);
const code = (v: unknown) => `<code>${esc(v)}</code>`;
const link = (path: string, text: string) => `<a href="${esc(`${REPO}/${path}`)}">${esc(text)}</a>`;

// ---- amounts -----------------------------------------------------------------

const inrFormat = new Intl.NumberFormat('en-IN', { maximumFractionDigits: 2, minimumFractionDigits: 0 });
export const inr = (minor: number) => `INR ${inrFormat.format(minor / 100)}`;

export interface Ledger { available: number; reserved: number; committed: number; unresolved: number; orders: number }

/** Parses the recorded ledger line, e.g. "available INR 4,00,000 | reserved INR 0 | ... | orders 1". */
export function parseLedger(s: string): Ledger | null {
  const amount = (k: string) => {
    const m = new RegExp(`${k} INR ([0-9,]+(?:\\.[0-9]{1,2})?)`).exec(s);
    return m ? Math.round(Number(m[1].replace(/,/g, '')) * 100) : NaN;
  };
  const orders = /orders (\d+)/.exec(s);
  const l = { available: amount('available'), reserved: amount('reserved'), committed: amount('committed'), unresolved: amount('unresolved'), orders: orders ? Number(orders[1]) : NaN };
  return Object.values(l).some(Number.isNaN) ? null : l;
}

export const START_LEDGER: Ledger = { available: START_LIMIT_MINOR, reserved: 0, committed: 0, unresolved: 0, orders: 0 };

function ledgerTable(l: Ledger, caption: string): string {
  const rows: [string, number][] = [['Available', l.available], ['Reserved', l.reserved], ['Committed', l.committed], ['Unresolved', l.unresolved]];
  return `<table class="ledger"><caption>${esc(caption)}</caption>
    <thead><tr><th scope="col">Bucket</th><th scope="col">Amount</th><th scope="col" class="dev">Minor units (paise)</th></tr></thead>
    <tbody>${rows.map(([k, v]) => `<tr><th scope="row">${k}</th><td>${esc(inr(v))}</td><td class="dev">${code(v)}</td></tr>`).join('')}
    <tr><th scope="row">Orders</th><td>${esc(l.orders)}</td><td class="dev"></td></tr></tbody></table>`;
}

// ---- commits and labels ------------------------------------------------------

export function describeCommit(c: string): { short: string; dirty: boolean; full: string } {
  const dirty = c.endsWith('-dirty');
  const full = dirty ? c.slice(0, -'-dirty'.length) : c;
  return { short: full.slice(0, 7), dirty, full };
}

const commitText = (c: string) => {
  const d = describeCommit(c);
  return `${code(d.short)}${d.dirty ? ' <span class="warn-text">(recorded from a working tree with uncommitted changes)</span>' : ''}`;
};

const OUTCOME: Record<Outcome, { label: string; mark: string; cls: string }> = {
  done: { label: 'Done', mark: '✓', cls: 'ok' },
  checked: { label: 'Checked', mark: '✓', cls: 'ok' },
  committed: { label: 'Committed', mark: '●', cls: 'committed' },
  denied: { label: 'Denied', mark: '✕', cls: 'denied' },
  'client-unknown': { label: 'Client unsure', mark: '?', cls: 'unknown' },
};

export function chip(cls: string, mark: string, text: string): string {
  return `<span class="chip ${esc(cls)}"><span aria-hidden="true">${esc(mark)}</span> ${esc(text)}</span>`;
}

// ---- shell ---------------------------------------------------------------------

export type Route =
  | { screen: 'overview' }
  | { screen: 'scenario'; card: string }
  | { screen: 'evidence'; case: string }
  | { screen: 'quickstart' };

export function parseRoute(hash: string, data: DemoData): Route {
  const [screen, arg] = hash.replace(/^#\/?/, '').split('/');
  if (screen === 'scenario' && CARDS.some((c) => c.id === arg)) return { screen: 'scenario', card: arg };
  if (screen === 'evidence') {
    const names = data.evidence.data.cases.map((c) => c.name);
    return { screen: 'evidence', case: names.includes(arg) ? arg : names[0] };
  }
  if (screen === 'quickstart') return { screen: 'quickstart' };
  return { screen: 'overview' };
}

export function header(data: DemoData): string {
  const ref = data.reference.data;
  const build = data.build;
  return `<header class="site">
    <div class="brand">
      <p class="title">Steelmatter procurement reference</p>
      <p class="labels">${chip('label', '◆', 'Simulation')} ${chip('label', '◆', 'Synthetic data')} ${chip('label', '▶', 'Recorded playback, not a live run')}</p>
    </div>
    <dl class="provenance">
      <div><dt>Recorded run</dt><dd>${commitText(ref.generated_at_commit)}</dd></div>
      <div><dt>Recording file</dt><dd>${code(data.reference.file)} <span class="dev">sha256 ${code(data.reference.sha256.slice(0, 12))}</span></dd></div>
      <div><dt>Page built from</dt><dd>${code(describeCommit(build.commit).short)}${build.dirty ? ' <span class="warn-text">(uncommitted changes)</span>' : ''}</dd></div>
    </dl>
  </header>`;
}

export function nav(route: Route): string {
  const item = (href: string, text: string, active: boolean) =>
    `<li><a href="${href}"${active ? ' aria-current="page"' : ''}>${esc(text)}</a></li>`;
  return `<nav aria-label="Screens"><ul>
    ${item('#/', 'Scenarios', route.screen === 'overview' || route.screen === 'scenario')}
    ${item('#/evidence', 'Evidence', route.screen === 'evidence')}
    ${item('#/quickstart', 'Developer quickstart', route.screen === 'quickstart')}
  </ul></nav>`;
}

// ---- screen A: overview --------------------------------------------------------

export const scenarioFor = (data: DemoData, card: Card): ReferenceScenario | undefined =>
  card.scenario ? data.reference.data.scenarios.find((s) => s.scenario === card.scenario) : undefined;

export function overview(data: DemoData): string {
  const ref = data.reference.data;
  const cards = CARDS.map((card) => {
    const s = scenarioFor(data, card);
    const tests = s ? s.test_ids : card.tests ?? [];
    const status = s
      ? chip(s.status === 'PASS' ? 'ok' : 'denied', s.status === 'PASS' ? '✓' : '✕', `Recorded result: ${s.status}`)
      : chip('unknown', '–', 'Not recorded yet');
    const action = s
      ? `<a class="button" href="#/scenario/${esc(card.id)}">Play ${esc(card.id)}<span class="visually-hidden"> ${esc(card.title)}</span></a>`
      : `<p class="note">No recorded run exists for this case yet, so it cannot be played. The case is covered by ${tests.map(code).join(', ')}; a demo scenario still has to be added to ${link('examples/procurement/scenarios.go', 'examples/procurement/scenarios.go')}.</p>`;
    return `<li class="card${s ? '' : ' unavailable'}">
      <h2><span class="case">${esc(card.id)}</span> ${esc(card.title)}${card.extra ? ' <span class="extra">additional</span>' : ''}</h2>
      <p>${esc(card.summary)}</p>
      <p>${status}</p>
      <details><summary>Prerequisites and source</summary>
        <ul>
          <li>Recorded by: ${code(ref.generated_by)}</li>
          ${s ? `<li>Scenario: ${code(s.scenario)} in ${link('examples/procurement/scenarios.go', 'scenarios.go')}</li>` : ''}
          <li>Tests: ${tests.map(code).join(', ')} (${link('tests/integration', 'tests/integration')})</li>
          ${s && s.fixtures.length ? `<li>Fixtures: ${s.fixtures.map((f) => `${code(f)} <span class="dev">sha256 ${code((ref.fixtures[f] ?? '').slice(0, 12))}</span>`).join(', ')}</li>` : ''}
        </ul>
      </details>
      ${action}
    </li>`;
  }).join('');
  return `<main id="main" tabindex="-1">
    <h1>Scenarios</h1>
    <p class="lede">Each card replays a recorded run of the reference against a real PostgreSQL database. ${esc(ref.note)}</p>
    <p><button type="button" data-action="reset">Reset to the synthetic starting state</button></p>
    <ul class="cards">${cards}</ul>
  </main>`;
}

// ---- screen B: scenario run ----------------------------------------------------

function stepItem(s: ReferenceScenario, i: number, focus?: string): string {
  const step = s.steps[i];
  const note: StepNote | undefined = STEP_NOTES[s.scenario]?.[step.name];
  const o = note ? OUTCOME[note.outcome] : OUTCOME.done;
  const focused = focus !== undefined && note?.cases?.includes(focus);
  const recorded = step.ok ? chip('ok', '✓', 'Recorded: passed') : chip('denied', '✕', 'Recorded: FAILED');
  return `<li class="step ${o.cls}${focused ? ' focus' : ''}" data-step="${i}">
    <p class="step-head"><span class="phase">${esc(note?.phase ?? 'Step')}</span> ${note?.lane ? `<span class="lane">${note.lane === 'client' ? 'Client view' : 'Database view'}</span>` : ''}
      <strong>${i + 1}. ${esc(step.name)}</strong></p>
    <p class="actor">${esc(note?.actor ?? '')}</p>
    <p>${esc(note?.detail ?? '')}</p>
    ${note?.change ? `<p class="change"><strong>Material change:</strong> ${esc(note.change)}</p>` : ''}
    <p class="outcome">${chip(o.cls, o.mark, note?.reason ? `${o.label}: ${note.reason}` : o.label)} ${recorded}${focused ? ` ${chip('focus', '★', `Shows ${focus}`)}` : ''}</p>
  </li>`;
}

export function scenarioView(data: DemoData, cardId: string, shown: number): string {
  const card = CARDS.find((c) => c.id === cardId)!;
  const s = scenarioFor(data, card);
  if (!s) return `<main id="main" tabindex="-1"><h1>${esc(card.id)} ${esc(card.title)}</h1><p class="note">No recorded run exists for this case yet.</p><p><a href="#/">Back to scenarios</a></p></main>`;
  const n = s.steps.length;
  const k = Math.max(0, Math.min(shown, n));
  const people = PEOPLE[s.scenario];
  const end = parseLedger(s.ledger);
  const state = k === n ? chip('ok', '■', `Finished: ${n} of ${n} steps`) : chip('working', '…', `Working: step ${k} of ${n}`);
  const lastNote = k > 0 ? STEP_NOTES[s.scenario]?.[s.steps[k - 1].name] : undefined;
  const announce = k > 0 ? `Step ${k} of ${n}: ${s.steps[k - 1].name}. ${lastNote?.reason ? `Denied: ${lastNote.reason}.` : ''}` : 'Ready to play.';
  return `<main id="main" tabindex="-1">
    <p><a href="#/">← Back to scenarios</a></p>
    <h1><span class="case">${esc(card.id)}</span> ${esc(card.title)}</h1>
    <p class="lede">${esc(card.summary)}</p>
    <section aria-labelledby="trace-h" class="trace">
      <h2 id="trace-h">Trace</h2>
      <dl>
        <div><dt>Status</dt><dd>${chip('label', '▶', 'Simulated, recorded playback')} ${chip(s.status === 'PASS' ? 'ok' : 'denied', s.status === 'PASS' ? '✓' : '✕', `Recorded result: ${s.status}`)}</dd></div>
        <div><dt>Case</dt><dd>${code(s.case)} (scenario ${code(s.scenario)})</dd></div>
        <div><dt>Tests</dt><dd>${s.test_ids.map(code).join(', ')}</dd></div>
        <div><dt>Fixtures</dt><dd>${s.fixtures.length ? s.fixtures.map((f) => `${code(f)} <span class="dev">sha256 ${code((data.reference.data.fixtures[f] ?? '').slice(0, 12))}</span>`).join(', ') : 'none'}</dd></div>
        <div><dt>Commit</dt><dd>${commitText(data.reference.data.generated_at_commit)}</dd></div>
      </dl>
    </section>
    <section aria-labelledby="who-h" class="who">
      <h2 id="who-h">Who is involved</h2>
      <ul>
        <li>Owner: ${code(people.owner)}, who signs the mandate (allowance)</li>
        <li>Agent${people.agents.length > 1 ? 's' : ''}: ${people.agents.map(code).join(' and ')}</li>
        <li>Supplier: ${code(people.supplier)}</li>
      </ul>
    </section>
    <section aria-labelledby="cap-h" class="capacity">
      <h2 id="cap-h">Capacity</h2>
      <div class="ledgers">
        ${ledgerTable(START_LEDGER, 'At the start')}
        ${k === n && end ? ledgerTable(end, 'At the end (recorded)') : `<p class="note">The recorded end state appears when playback reaches the end. The recording stores the start and end amounts, not every step in between.</p>`}
      </div>
    </section>
    <section aria-labelledby="run-h" class="run">
      <h2 id="run-h">Run</h2>
      <p>${state}</p>
      <div class="controls" role="group" aria-label="Playback">
        <button type="button" data-action="restart" ${k === 0 ? 'disabled' : ''}>Restart</button>
        <button type="button" data-action="prev" ${k === 0 ? 'disabled' : ''}>Previous step</button>
        <button type="button" data-action="next" ${k === n ? 'disabled' : ''}>Next step</button>
        <button type="button" data-action="play" ${k === n ? 'disabled' : ''}>Play to the end</button>
      </div>
      <p class="phases">Phases: ${PHASES.map(esc).join(' → ')}</p>
      <ol class="timeline">${s.steps.slice(0, k).map((_, i) => stepItem(s, i, card.focus)).join('')}</ol>
      ${k < n ? `<p class="note">${n - k} more step${n - k === 1 ? '' : 's'} to play.</p>` : ''}
      <p class="visually-hidden" role="status" aria-live="polite">${esc(announce)}</p>
    </section>
  </main>`;
}

// ---- screen C: evidence --------------------------------------------------------

export interface EvidenceResult { atFixture: EvidenceReport; now: EvidenceReport }
export type EvidenceState =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ok'; result: EvidenceResult };

function decodePayload(jws: unknown): Record<string, unknown> | null {
  if (typeof jws !== 'string') return null;
  try {
    const p = jws.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
    const bin = atob(p + '='.repeat((4 - (p.length % 4)) % 4));
    return JSON.parse(new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0))));
  } catch {
    return null;
  }
}

export function overallEvidence(r: EvidenceReport): { cls: string; mark: string; text: string } {
  if (r.chronology?.status === 'INCONSISTENT') return { cls: 'denied', mark: '✕', text: 'Evidence mismatch: impossible timeline' };
  if (r.linkage.status === 'BROKEN') return { cls: 'denied', mark: '✕', text: 'Evidence mismatch: broken linkage' };
  if (r.observation.status === 'MISMATCH') return { cls: 'denied', mark: '✕', text: 'Evidence mismatch: observed order differs' };
  if (r.linkage.status === 'INCOMPLETE' || r.observation.status !== 'MATCH') return { cls: 'unknown', mark: '…', text: 'Evidence incomplete' };
  return { cls: 'ok', mark: '✓', text: 'Evidence complete and consistent' };
}

const okCls = (v: string, good: string[]) => (good.includes(v) ? 'ok' : v === 'MISSING' || v === 'UNVERIFIED' || v === 'UNKNOWN' ? 'unknown' : 'denied');
const recChip = (v: string, good: string[]) => chip(okCls(v, good), okCls(v, good) === 'ok' ? '✓' : okCls(v, good) === 'unknown' ? '–' : '✕', v);

function expectationRows(c: EvidenceCase, r: EvidenceReport): string {
  const actual: Record<string, unknown> = {
    linkage: r.linkage.status,
    observation: r.observation.status,
    observed_matches_action: r.observation.observed_matches_action,
    broken_check: r.linkage.checks.find((x) => x.status !== 'OK')?.name,
  };
  return Object.entries(c.expect).map(([k, want]) => {
    const got = actual[k];
    const same = String(got) === String(want);
    return `<tr><th scope="row">${esc(k)}</th><td>${code(want)}</td><td>${code(got)}</td><td>${same ? chip('ok', '✓', 'Matches') : chip('denied', '✕', 'Differs')}</td></tr>`;
  }).join('');
}

export function evidenceView(data: DemoData, caseName: string, state: EvidenceState | undefined): string {
  const fx = data.evidence.data;
  const c = fx.cases.find((x) => x.name === caseName) ?? fx.cases[0];
  const tabs = fx.cases.map((x) => {
    const l = EVIDENCE_LABELS[x.name] ?? { title: x.name, summary: '' };
    return `<li><a href="#/evidence/${esc(x.name)}"${x.name === c.name ? ' aria-current="true"' : ''}>${esc(l.title)}</a></li>`;
  }).join('');
  const label = EVIDENCE_LABELS[c.name] ?? { title: c.name, summary: '' };
  let body: string;
  if (!state || state.status === 'loading') {
    body = `<p role="status">${chip('working', '…', 'Working: checking signatures and links in this browser')}</p>`;
  } else if (state.status === 'error') {
    body = `<div role="alert" class="error"><p>${chip('denied', '✕', 'The check could not run')} ${esc(state.message)}</p>
      <p><button type="button" data-action="retry-evidence">Try again</button></p></div>`;
  } else {
    const r = state.result.atFixture;
    const now = state.result.now;
    const o = overallEvidence(r);
    const payloads = Object.fromEntries((['mandate', 'permit', 'execution', 'observation'] as const).map((k) => [k, decodePayload(c.bundle.records[`${k}_jws`])]));
    const action = (c.bundle as { action?: Record<string, unknown> }).action ?? {};
    const keyName = (kid?: string) => {
      const keys = (fx.trust_store as { keys?: { kid: string; name: string }[] }).keys ?? [];
      return keys.find((k) => k.kid === kid)?.name;
    };
    const field = (label: string, value: unknown) => (value === undefined || value === null ? '' : `<div><dt>${esc(label)}</dt><dd>${code(value)}</dd></div>`);
    body = `
      <p class="overall">${chip(o.cls, o.mark, o.text)}</p>
      <section aria-labelledby="sep-h"><h3 id="sep-h">Results, kept separate</h3>
        <dl class="results">
          <div><dt>Signature integrity</dt><dd>${(['mandate', 'permit', 'execution', 'observation'] as const).map((k) => `${esc(k)} ${recChip(r.records[k].integrity, ['VALID'])}`).join(' ')}</dd></div>
          <div><dt>Trusted issuer</dt><dd>${(['mandate', 'permit', 'execution', 'observation'] as const).map((k) => `${esc(k)} ${recChip(r.records[k].issuer, ['TRUSTED'])}`).join(' ')}</dd></div>
          <div><dt>Execution state</dt><dd>${payloads.execution ? `${code(payloads.execution.status)} (signed execution record)` : chip('unknown', '–', 'No signed execution record')}</dd></div>
          <div><dt>Observation comparison</dt><dd>${recChip(r.observation.status, ['MATCH'])}</dd></div>
          <div><dt>Linkage</dt><dd>${recChip(r.linkage.status, ['COMPLETE'])}</dd></div>
          <div><dt>Chronology</dt><dd>${recChip(r.chronology.status, ['CONSISTENT'])}${r.chronology.checks.filter((x) => x.status === 'MISMATCH').map((x) => ` ${code(x.name)} ${esc(x.detail ?? '')}`).join('')}</dd></div>
        </dl>
      </section>
      <section aria-labelledby="what-h"><h3 id="what-h">What the evidence says</h3>
        <dl class="fields">
          ${field('Instruction', action.instruction_id ?? payloads.permit?.instruction_id)}
          ${field('Action digest', r.action.digest)}
          ${field('Mandate', payloads.mandate?.mandate_id)}
          ${field('Mandate revision', payloads.mandate?.revision ?? payloads.permit?.mandate_revision)}
          ${field('Permit', payloads.permit?.permit_id)}
          ${field('Operation', payloads.execution?.operation_id ?? payloads.permit?.operation_id)}
          ${field('Obligation', payloads.execution?.obligation_id)}
          ${field('Amount', typeof action.amount_minor === 'number' ? `${inr(action.amount_minor)} (${action.amount_minor} paise)` : undefined)}
          ${field('Observed effect', payloads.observation ? `${payloads.observation.status}; observed digest ${payloads.observation.observed_action_digest ?? 'none'}` : undefined)}
        </dl>
      </section>
      <section aria-labelledby="rec-h"><h3 id="rec-h">Records and provenance</h3>
        <table><thead><tr><th scope="col">Record</th><th scope="col">Integrity</th><th scope="col">Issuer</th><th scope="col">Profile</th><th scope="col">Payload</th><th scope="col">Signing key</th></tr></thead>
        <tbody>${(['mandate', 'permit', 'execution', 'observation'] as const).map((k) => {
          const x = r.records[k];
          return `<tr><th scope="row">${esc(k)}</th><td>${recChip(x.integrity, ['VALID'])}</td><td>${recChip(x.issuer, ['TRUSTED'])}</td><td>${recChip(x.profile, ['SUPPORTED'])}</td><td>${recChip(x.payload, ['VALID'])}${x.code ? ` ${code(x.code)}` : ''}</td><td>${x.kid ? `${esc(keyName(x.kid) ?? 'unknown key')} <span class="dev">${code(x.kid.slice(0, 12))}</span>` : '–'}</td></tr>`;
        }).join('')}</tbody></table>
        <h4>Linkage checks</h4>
        <ul class="checks">${r.linkage.checks.map((x) => `<li>${recChip(x.status, ['OK'])} ${code(x.name)}${x.detail ? ` ${esc(x.detail)}` : ''}</li>`).join('')}</ul>
      </section>
      <section aria-labelledby="use-h"><h3 id="use-h">Usable now, or historical evidence?</h3>
        <table><thead><tr><th scope="col">Checked at</th><th scope="col">Permit</th><th scope="col">Mandate</th><th scope="col">Signatures</th></tr></thead>
        <tbody>
          <tr><th scope="row">Inside the permit window (${code(fx.usable_at)})</th><td>${recChip(r.usability.permit, ['USABLE'])}</td><td>${recChip(r.usability.mandate, ['WITHIN_WINDOW'])}</td><td>${recChip(r.records.permit.integrity, ['VALID'])}</td></tr>
          <tr><th scope="row">Now (${code(now.evaluated_at)})</th><td>${recChip(now.usability.permit, ['USABLE'])}</td><td>${recChip(now.usability.mandate, ['WITHIN_WINDOW'])}</td><td>${recChip(now.records.permit.integrity, ['VALID'])}</td></tr>
        </tbody></table>
        <p class="note">${now.records.permit.integrity === 'VALID' && now.usability.permit === 'EXPIRED' ? 'The permit still verifies as authentic evidence, but it has expired and cannot be used for a new action. ' : ''}${esc(r.usability.note)}</p>
      </section>
      <section aria-labelledby="exp-h"><h3 id="exp-h">Fixture expectation compared with this browser's result</h3>
        <table><thead><tr><th scope="col">Check</th><th scope="col">Fixture expects</th><th scope="col">Browser verifier</th><th scope="col">Result</th></tr></thead>
        <tbody>${expectationRows(c, r)}</tbody></table>
      </section>
      <section aria-labelledby="claim-h"><h3 id="claim-h">Service claims (reported, not trusted)</h3>
        <p>The bundle says evidence is ${code(JSON.stringify(c.bundle.evidence ?? null))} and incomplete parts are ${code(JSON.stringify(c.bundle.incomplete ?? null))}. ${esc(r.service_claims.note)}</p>
      </section>
      <p><button type="button" data-action="download-evidence">Download this synthetic evidence (JSON)</button></p>`;
  }
  return `<main id="main" tabindex="-1">
    <h1>Evidence</h1>
    <p class="lede">These synthetic evidence chains come from the verifier fixture set ${code(data.evidence.file)} (${esc(fx.fixture_set)}), built from the packet examples. They are not produced by the scenario runs. Each one is checked here, in this browser, by the standalone verifier (${link('packages/verifier', 'packages/verifier')}, bundled unchanged). No network call is made.</p>
    <nav aria-label="Evidence chains"><ul class="tabs">${tabs}</ul></nav>
    <h2>${esc(label.title)}</h2>
    <p>${esc(label.summary)}</p>
    ${body}
  </main>`;
}

// ---- screen D: developer quickstart -----------------------------------------------

export function quickstart(data: DemoData): string {
  return `<main id="main" tabindex="-1">
    <h1>Developer quickstart</h1>
    <section aria-labelledby="run-local"><h2 id="run-local">Run the reference locally</h2>
      <p>Needs Go, Node and a local PostgreSQL. No Claude or model API key is needed for any deterministic test.</p>
      <pre><code>STEELMATTER_ADMIN_URL='postgres://user@localhost:5432/postgres?sslmode=disable' scripts/demo</code></pre>
      <p>Runs every scenario from a clean state (fresh keys, fresh database), verifies the flagship evidence offline, then tears everything down. Full test run with the aggregate gate: ${code('scripts/ci')}.</p>
    </section>
    <section aria-labelledby="src"><h2 id="src">Inspect source and tests</h2>
      <ul>
        <li>Scenarios: ${link('examples/procurement/scenarios.go', 'examples/procurement/scenarios.go')}</li>
        <li>Integration tests: ${link('tests/integration', 'tests/integration')}</li>
        <li>Acceptance cases: ${link('ci/acceptance-cases.json', 'ci/acceptance-cases.json')}, required results: ${link('ci/required.json', 'ci/required.json')}</li>
        <li>This page: ${link('demo', 'demo')}; rebuild with ${code('cd demo && npm ci && npm run build')}</li>
      </ul>
    </section>
    <section aria-labelledby="ver"><h2 id="ver">Install the verifier</h2>
      <pre><code>cd packages/verifier &amp;&amp; npm ci &amp;&amp; npm run build &amp;&amp; npm pack
npm install ./irontrex-steelmatter-verifier-0.0.0.tgz   # in your project
npx steelmatter-verify verify --trust TRUST_STORE.json --bundle EVIDENCE.json</code></pre>
      <p>The verifier never fetches keys: trust anchors are always given explicitly.</p>
    </section>
    <section aria-labelledby="lim"><h2 id="lim">Limitations</h2>
      <ul>
        <li>Simulation with synthetic data. No real ERP, supplier, bank, payment or customer system is connected.</li>
        <li>This page replays a recorded run (${commitText(data.reference.data.generated_at_commit)}); it does not execute anything live.</li>
        <li>The recording stores step names, results and the end ledger; the step explanations come from the scenario source.</li>
        <li>The evidence screen uses the verifier's synthetic fixture chains, not evidence from the recorded scenario runs.</li>
        <li>No public login, onboarding or per-action approval. Owner and operator actions are CLI-only and local.</li>
      </ul>
    </section>
    <section aria-labelledby="contact"><h2 id="contact">Contact for evaluation</h2>
      <p>The evaluation contact is not set yet; it is decided with publication (backlog I13).</p>
    </section>
  </main>`;
}

export function page(data: DemoData, route: Route, view: { shown: number; evidence?: EvidenceState }): string {
  const main = route.screen === 'scenario' ? scenarioView(data, route.card, view.shown)
    : route.screen === 'evidence' ? evidenceView(data, route.case, view.evidence)
    : route.screen === 'quickstart' ? quickstart(data)
    : overview(data);
  return `<a class="skip" href="#main" data-action="skip">Skip to content</a>${header(data)}${nav(route)}${main}
    <footer><p>Steelmatter R0 reference. ${esc(data.reference.data.note)}</p></footer>`;
}
