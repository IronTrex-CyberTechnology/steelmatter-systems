import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SteelmatterClient, SteelmatterResponseError } from '../src/index.ts';
import { responseFixtures } from './response-fixtures.ts';

type Path = (string | number)[];
function nodes(value: any, path: Path = []): { path: Path; value: any }[] {
  return [{path, value}, ...(value !== null && typeof value === 'object'
    ? Object.entries(value).flatMap(([key, child]) => nodes(child, [...path, Array.isArray(value) ? Number(key) : key])) : [])];
}
function replace(root: any, path: Path, value: any) {
  if (!path.length) return value;
  const copy = structuredClone(root);
  let parent = copy;
  for (const key of path.slice(0, -1)) parent = parent[key];
  parent[path.at(-1)!] = value;
  return copy;
}

for (const [operation, fixture] of Object.entries(responseFixtures)) {
  test(`adversarial ${operation}: recursively reject wrong types and extra object members`, async t => {
    let checked = 0;
    async function reject(body: any, label: string) {
      let requests = 0;
      const client = new SteelmatterClient({baseUrl: 'https://synthetic.invalid', token: 'synthetic',
        retry: {maxAttempts: 3, baseDelayMs: 0, maxDelayMs: 0},
        fetch: async () => { requests++; return new Response(JSON.stringify(body), {
          status: operation === 'createMandate' ? 201 : 200, headers: {'content-type': 'application/json'},
        }); }});
      await assert.rejects((client as any)[operation]('synthetic', {}), SteelmatterResponseError, label);
      assert.equal(requests, 1, `${label}: invalid response must not retry`);
      checked++;
    }
    for (const {path, value} of nodes(fixture)) {
      const wrong = typeof value === 'string' ? 1 : typeof value === 'number' ? '1'
        : typeof value === 'boolean' ? 'true' : Array.isArray(value) ? {} : [];
      await reject(replace(fixture, path, wrong), `wrong type at /${path.join('/')}`);
      if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
        for (const key of ['unexpected', '__proto__', 'constructor']) {
          const extra = JSON.parse(JSON.stringify(value));
          Object.defineProperty(extra, key, {value: {approved: true}, enumerable: true});
          await reject(replace(fixture, path, extra), `unknown ${key} at /${path.join('/')}`);
        }
      }
    }
    assert.ok(checked >= 4);
    t.diagnostic(`${checked} deterministic response mutations rejected without retries`);
  });
}
