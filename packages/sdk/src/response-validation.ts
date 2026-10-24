// Structural response validation against the published, closed API schemas.
// This does not verify signatures or establish the truth of service claims.
import { operationResponses, responseSchemas } from './response-schemas.ts';

export interface Schema {
  $ref?: string; type?: string; const?: unknown; enum?: unknown[];
  properties?: Record<string, Schema>; required?: string[]; additionalProperties?: boolean;
  items?: Schema; minItems?: number; maxItems?: number; uniqueItems?: boolean;
  minLength?: number; maxLength?: number; pattern?: string; format?: string;
  minimum?: number; maximum?: number; oneOf?: Schema[]; allOf?: Schema[];
}

const patterns = new Map<string, RegExp>();
function matches(schema: Schema, value: unknown, depth = 0): boolean {
  if (depth > 64) return false;
  const next = (s: Schema, v: unknown) => matches(s, v, depth + 1);
  if (schema.$ref) {
    const ref = responseSchemas[schema.$ref];
    if (!ref || !next(ref, value)) return false;
  }
  if ('const' in schema && value !== schema.const) return false;
  if (schema.enum && !schema.enum.includes(value)) return false;
  if (schema.oneOf && schema.oneOf.filter(s => next(s, value)).length !== 1) return false;
  if (schema.allOf && !schema.allOf.every(s => next(s, value))) return false;
  switch (schema.type) {
    case 'object': {
      if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
      const obj = value as Record<string, unknown>, props = schema.properties ?? {};
      if (schema.required?.some(k => !Object.hasOwn(obj, k))) return false;
      for (const key of Object.keys(obj)) {
        if (!Object.hasOwn(props, key)) { if (schema.additionalProperties === false) return false; }
        else if (!next(props[key]!, obj[key])) return false;
      }
      break;
    }
    case 'array':
      if (!Array.isArray(value)) return false;
      if (schema.minItems !== undefined && value.length < schema.minItems) return false;
      if (schema.maxItems !== undefined && value.length > schema.maxItems) return false;
      // All uniqueItems arrays in the response contract contain scalar identifiers.
      if (schema.uniqueItems && new Set(value).size !== value.length) return false;
      if (schema.items && !value.every(v => next(schema.items!, v))) return false;
      break;
    case 'string': {
      if (typeof value !== 'string') return false;
      // Count Unicode code points without allocating an array for the body.
      let length = 0;
      for (const _ of value) {
        length++;
        if (schema.maxLength !== undefined && length > schema.maxLength) return false;
      }
      if (schema.minLength !== undefined && length < schema.minLength) return false;
      if (schema.pattern) {
        let re = patterns.get(schema.pattern);
        if (!re) { re = new RegExp(schema.pattern, 'u'); patterns.set(schema.pattern, re); }
        if (!re.test(value)) return false;
      }
      if (schema.format === 'date-time') {
        // R0 timestamps are whole-second UTC; reject impossible calendar dates.
        const ms = Date.parse(value);
        if (!Number.isFinite(ms) || new Date(ms).toISOString() !== value.replace(/Z$/, '.000Z')) return false;
      }
      break;
    }
    case 'integer':
      if (typeof value !== 'number' || !Number.isSafeInteger(value)) return false;
      if (schema.minimum !== undefined && value < schema.minimum) return false;
      if (schema.maximum !== undefined && value > schema.maximum) return false;
      break;
    case 'boolean': if (typeof value !== 'boolean') return false; break;
    case undefined: break;
    default: return false;
  }
  return true;
}

export function validResponse(operation: string, status: number, value: unknown): boolean {
  const schema = operationResponses[operation]?.[String(status)];
  return schema !== undefined && matches(schema, value);
}
export function validError(value: unknown): boolean {
  return matches(responseSchemas.Error!, value);
}
