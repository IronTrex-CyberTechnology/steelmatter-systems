// Shared helpers for the demo tests: the real embedded data (from the pinned
// fixtures) and isolated build output directories.
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
// @ts-ignore: build.mjs is plain JavaScript
import { buildSite, buildVerifierBundle, demoData } from '../build.mjs';
import type { DemoData } from '../src/types.ts';

export const data = demoData() as DemoData;
export const tempDir = (p: string) => mkdtempSync(join(tmpdir(), `smdemo-${p}-`));
export { buildSite, buildVerifierBundle };
