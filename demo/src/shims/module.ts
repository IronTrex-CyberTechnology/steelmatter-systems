// Browser stand-in for node:module. payloads.ts loads Ajv with
// createRequire(import.meta.url); the same two packages (resolved from the
// verifier's own pinned node_modules) are bundled here instead.
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const modules: Record<string, unknown> = {
  'ajv/dist/2020.js': { default: Ajv2020 },
  'ajv-formats': { default: addFormats },
};

export function createRequire(_from: string | URL) {
  return (id: string) => {
    if (!(id in modules)) throw new Error(`module not available in the browser bundle: ${id}`);
    return modules[id];
  };
}
