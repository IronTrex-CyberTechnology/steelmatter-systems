// Browser stand-in for node:fs. The verifier reads its five JSON Schemas with
// readFileSync(new URL('../schemas/<kind>.schema.json', import.meta.url));
// the same schema files are bundled here at build time, unchanged.
import mandate from '../../../packages/verifier/schemas/mandate.schema.json' with { type: 'json' };
import action from '../../../packages/verifier/schemas/action.schema.json' with { type: 'json' };
import permit from '../../../packages/verifier/schemas/permit.schema.json' with { type: 'json' };
import execution from '../../../packages/verifier/schemas/execution.schema.json' with { type: 'json' };
import observation from '../../../packages/verifier/schemas/observation.schema.json' with { type: 'json' };

const schemas: Record<string, unknown> = { mandate, action, permit, execution, observation };

export function readFileSync(path: string | URL): string {
  const m = /\/schemas\/([a-z]+)\.schema\.json$/.exec(String(path));
  if (!m || !(m[1] in schemas)) throw new Error(`file not available in the browser bundle: ${String(path)}`);
  return JSON.stringify(schemas[m[1]]);
}

export function writeFileSync(): never {
  throw new Error('writeFileSync is not available in the browser');
}
