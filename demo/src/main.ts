// Browser entry: hash routing, playback state and in-browser evidence checks.
import raw from 'steelmatter:demo-data';
import { loadTrustStore, verifyBundle, type TrustStore } from './verifier.ts';
import { page, parseRoute, scenarioFor, type EvidenceState, type Route } from './render.ts';
import { CARDS } from './notes.ts';
import type { DemoData } from './types.ts';

const data = raw as DemoData;
const app = document.getElementById('app')!;
const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

let route: Route = parseRoute(location.hash, data);
let shown = 0;
let timer: number | undefined;
const evidence = new Map<string, EvidenceState>();
let trustPromise: Promise<TrustStore> | undefined;

function stopPlay() {
  if (timer !== undefined) window.clearInterval(timer);
  timer = undefined;
}

function stepCount(): number {
  if (route.screen !== 'scenario') return 0;
  const id = route.card;
  const s = scenarioFor(data, CARDS.find((c) => c.id === id)!);
  return s ? s.steps.length : 0;
}

function render(focusMain = false) {
  app.innerHTML = page(data, route, { shown, evidence: route.screen === 'evidence' ? evidence.get(route.case) : undefined });
  if (focusMain) document.getElementById('main')?.focus();
}

async function checkEvidence(name: string) {
  const c = data.evidence.data.cases.find((x) => x.name === name);
  if (!c) return;
  evidence.set(name, { status: 'loading' });
  render();
  try {
    trustPromise ??= loadTrustStore(data.evidence.data.trust_store as never);
    const trust = await trustPromise;
    const at = Math.floor(Date.parse(data.evidence.data.usable_at) / 1000);
    const [atFixture, now] = await Promise.all([verifyBundle(c.bundle as never, trust, at), verifyBundle(c.bundle as never, trust)]);
    evidence.set(name, { status: 'ok', result: { atFixture, now } });
  } catch (e) {
    trustPromise = undefined;
    evidence.set(name, { status: 'error', message: e instanceof Error ? e.message : String(e) });
  }
  if (route.screen === 'evidence' && route.case === name) render();
}

function navigate() {
  stopPlay();
  const next = parseRoute(location.hash, data);
  const changedScenario = next.screen !== 'scenario' || route.screen !== 'scenario' || next.card !== route.card;
  route = next;
  if (changedScenario) shown = 0;
  render(true);
  if (route.screen === 'evidence' && !evidence.has(route.case)) void checkEvidence(route.case);
}

function download(name: string) {
  const c = data.evidence.data.cases.find((x) => x.name === name);
  if (!c) return;
  const blob = new Blob([JSON.stringify(c.bundle, null, 2) + '\n'], { type: 'application/json' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `steelmatter-synthetic-evidence-${name}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

app.addEventListener('click', (ev) => {
  const el = (ev.target as HTMLElement).closest<HTMLElement>('[data-action]');
  if (!el) return;
  const n = stepCount();
  switch (el.dataset.action) {
    case 'skip':
      ev.preventDefault();
      document.getElementById('main')?.focus();
      return;
    case 'reset':
      stopPlay();
      shown = 0;
      evidence.clear();
      trustPromise = undefined;
      if (location.hash && location.hash !== '#/') location.hash = '#/';
      else render(true);
      return;
    case 'restart': stopPlay(); shown = 0; break;
    case 'prev': stopPlay(); shown = Math.max(0, shown - 1); break;
    case 'next': stopPlay(); shown = Math.min(n, shown + 1); break;
    case 'play':
      if (reducedMotion) {
        shown = n;
        break;
      }
      stopPlay();
      timer = window.setInterval(() => {
        shown = Math.min(n, shown + 1);
        if (shown >= n) stopPlay();
        render();
        if (shown >= n) app.querySelector<HTMLElement>('[data-action="restart"]')?.focus();
      }, 900);
      return;
    case 'retry-evidence':
      if (route.screen === 'evidence') void checkEvidence(route.case);
      return;
    case 'download-evidence':
      if (route.screen === 'evidence') download(route.case);
      return;
    default:
      return;
  }
  render();
  const again = app.querySelector<HTMLElement>(`[data-action="${el.dataset.action}"]:not([disabled])`)
    ?? app.querySelector<HTMLElement>('.controls button:not([disabled])');
  again?.focus();
});

window.addEventListener('hashchange', navigate);
render();
if (route.screen === 'evidence') void checkEvidence(route.case);
