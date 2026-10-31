// Builds the browser demo into dist/ (or the directory given with --out):
//   index.html, style.css, app.js  - open index.html directly; no server needed
//   verifier-browser.mjs            - the same browser verifier bundle as an
//                                     ES module, used by the demo tests
//
// The verifier is bundled from packages/verifier/src unchanged. Its few Node
// calls (node:crypto sha256, node:fs schema reads, node:module for Ajv and
// Buffer for base64url) are served by small browser stand-ins in src/shims.
// Demo data is embedded from the pinned fixtures at build time:
//   fixtures/demo/reference-results-v1.json  (recorded scenario run, T43)
//   fixtures/vectors/evidence-v1.json        (synthetic evidence chains)
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, readFileSync, realpathSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, '..');
const verifierDir = join(root, 'packages', 'verifier');

export const FIXTURES = {
  reference: 'fixtures/demo/reference-results-v1.json',
  evidence: 'fixtures/vectors/evidence-v1.json',
};

function git(dir, args) {
  try {
    return execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return '';
  }
}

/**
 * The commit the page is built from (T43): HEAD of the checkout at dir, dirty
 * when tracked files have uncommitted changes; in a release bundle, which has
 * no .git, the commit scripts/release-bundle wrote to COMMIT; else unknown. A
 * bundle unpacked inside another repository never takes that one's HEAD.
 */
export function sourceCommit(dir = root) {
  const top = git(dir, ['rev-parse', '--show-toplevel']);
  if (top && realpathSync(top) === realpathSync(dir)) {
    const commit = git(dir, ['rev-parse', 'HEAD']);
    if (commit) return { commit, dirty: git(dir, ['status', '--porcelain', '--untracked-files=no']) !== '' };
  }
  try {
    const commit = readFileSync(join(dir, 'COMMIT'), 'utf8').trim();
    if (/^[0-9a-f]{40}$/.test(commit)) return { commit, dirty: false };
  } catch {
    // no COMMIT file
  }
  return { commit: 'unknown', dirty: false };
}

/** The data the page embeds: pinned fixtures plus their digests and the build commit. */
export function demoData() {
  const load = (rel) => {
    const raw = readFileSync(join(root, rel));
    return { file: rel, sha256: createHash('sha256').update(raw).digest('hex'), data: JSON.parse(raw.toString('utf8')) };
  };
  return {
    reference: load(FIXTURES.reference),
    evidence: load(FIXTURES.evidence),
    build: sourceCommit(),
  };
}

const shims = {
  name: 'steelmatter-browser-shims',
  setup(b) {
    const shim = (f) => ({ path: join(here, 'src', 'shims', f) });
    b.onResolve({ filter: /^node:crypto$/ }, () => shim('crypto.ts'));
    b.onResolve({ filter: /^node:fs$/ }, () => shim('fs.ts'));
    b.onResolve({ filter: /^node:module$/ }, () => shim('module.ts'));
    b.onResolve({ filter: /^steelmatter:demo-data$/ }, () => ({ path: 'demo-data', namespace: 'steelmatter' }));
    b.onLoad({ filter: /.*/, namespace: 'steelmatter' }, () => ({ contents: JSON.stringify(demoData()), loader: 'json' }));
  },
};

const common = {
  bundle: true,
  platform: 'browser',
  target: 'es2022',
  plugins: [shims],
  inject: [join(here, 'src', 'shims', 'buffer.ts')],
  nodePaths: [join(verifierDir, 'node_modules'), join(here, 'node_modules')],
  // payloads.ts resolves its schema files against import.meta.url; give it a
  // stable URL that the fs stand-in maps back to the bundled schemas.
  define: { 'import.meta.url': JSON.stringify('file:///steelmatter/packages/verifier/src/payloads.ts') },
  logLevel: 'warning',
  legalComments: 'none',
};

/** The browser verifier as an ES module (for tests). */
export async function buildVerifierBundle(outfile) {
  await build({ ...common, entryPoints: [join(here, 'src', 'verifier.ts')], format: 'esm', outfile });
  return outfile;
}

/** The whole site. */
export async function buildSite(outdir) {
  mkdirSync(outdir, { recursive: true });
  await build({ ...common, entryPoints: [join(here, 'src', 'main.ts')], format: 'iife', outfile: join(outdir, 'app.js') });
  await buildVerifierBundle(join(outdir, 'verifier-browser.mjs'));
  copyFileSync(join(here, 'src', 'index.html'), join(outdir, 'index.html'));
  copyFileSync(join(here, 'src', 'style.css'), join(outdir, 'style.css'));
  return outdir;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const i = process.argv.indexOf('--out');
  const out = resolve(i > 0 ? process.argv[i + 1] : join(here, 'dist'));
  await buildSite(out);
  console.log(`built ${out}`);
}
