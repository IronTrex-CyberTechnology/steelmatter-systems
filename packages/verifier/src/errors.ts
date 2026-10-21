// Stable rejection codes shared with the Go engine and the golden vectors
// (docs/contracts/I02-contract.md). Keep in sync with core/contract/errors.go.
export type Code =
  | 'PAYLOAD_TOO_LARGE' | 'INVALID_ENCODING' | 'INVALID_JSON' | 'DUPLICATE_KEY'
  | 'NON_INTEGER_NUMBER' | 'NUMBER_OUT_OF_RANGE' | 'UNSUPPORTED_VERSION' | 'UNSUPPORTED_PROFILE'
  | 'UNKNOWN_FIELD' | 'UNSUPPORTED_CURRENCY' | 'INVALID_TIMESTAMP' | 'SCHEMA_VIOLATION'
  | 'DUPLICATE_LINE_ITEM' | 'LINES_NOT_CANONICAL' | 'ARITHMETIC_OVERFLOW' | 'AMOUNT_MISMATCH'
  | 'INVALID_TIME_RANGE' | 'OBSERVATION_INCONSISTENT'
  | 'JWS_MALFORMED' | 'JWS_ALG_UNSUPPORTED' | 'JWS_HEADER_UNSUPPORTED' | 'JWS_TYPE_MISMATCH'
  | 'JWS_KEY_UNTRUSTED' | 'JWS_SIGNATURE_INVALID' | 'JWS_PAYLOAD_NOT_CANONICAL';

export class ContractError extends Error {
  readonly code: Code;
  readonly path: string;
  constructor(code: Code, path: string, detail: string) {
    super(`${code}${path ? ` at ${path}` : ''}: ${detail}`);
    this.code = code;
    this.path = path;
  }
}
