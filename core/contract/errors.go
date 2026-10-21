// Package contract implements the I02 payload contract: strict JSON input,
// payload validation, RFC 8785 canonical bytes, digests and the SM-JWS-ES256-1
// signing profile. The rules and their check order are specified in
// docs/contracts/I02-contract.md and mirrored by packages/verifier.
package contract

import "fmt"

// Code is a stable rejection code shared with the TypeScript verifier and
// the golden vectors.
type Code string

// Input codes, in the order checks run (docs/contracts/I02-contract.md §2).
const (
	CodePayloadTooLarge         Code = "PAYLOAD_TOO_LARGE"
	CodeInvalidEncoding         Code = "INVALID_ENCODING"
	CodeInvalidJSON             Code = "INVALID_JSON"
	CodeDuplicateKey            Code = "DUPLICATE_KEY"
	CodeNonIntegerNumber        Code = "NON_INTEGER_NUMBER"
	CodeNumberOutOfRange        Code = "NUMBER_OUT_OF_RANGE"
	CodeUnsupportedVersion      Code = "UNSUPPORTED_VERSION"
	CodeUnsupportedProfile      Code = "UNSUPPORTED_PROFILE"
	CodeUnknownField            Code = "UNKNOWN_FIELD"
	CodeUnsupportedCurrency     Code = "UNSUPPORTED_CURRENCY"
	CodeInvalidTimestamp        Code = "INVALID_TIMESTAMP"
	CodeSchemaViolation         Code = "SCHEMA_VIOLATION"
	CodeDuplicateLineItem       Code = "DUPLICATE_LINE_ITEM"
	CodeLinesNotCanonical       Code = "LINES_NOT_CANONICAL"
	CodeArithmeticOverflow      Code = "ARITHMETIC_OVERFLOW"
	CodeAmountMismatch          Code = "AMOUNT_MISMATCH"
	CodeInvalidTimeRange        Code = "INVALID_TIME_RANGE"
	CodeObservationInconsistent Code = "OBSERVATION_INCONSISTENT"
)

// JWS codes (docs/contracts/I02-contract.md §5).
const (
	CodeJWSMalformed           Code = "JWS_MALFORMED"
	CodeJWSAlgUnsupported      Code = "JWS_ALG_UNSUPPORTED"
	CodeJWSHeaderUnsupported   Code = "JWS_HEADER_UNSUPPORTED"
	CodeJWSTypeMismatch        Code = "JWS_TYPE_MISMATCH"
	CodeJWSKeyUntrusted        Code = "JWS_KEY_UNTRUSTED"
	CodeJWSSignatureInvalid    Code = "JWS_SIGNATURE_INVALID"
	CodeJWSPayloadNotCanonical Code = "JWS_PAYLOAD_NOT_CANONICAL"
)

// Error is a contract rejection. Path is a JSON Pointer when applicable.
type Error struct {
	Code   Code
	Path   string
	Detail string
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func errf(code Code, path, format string, a ...any) *Error {
	return &Error{Code: code, Path: path, Detail: fmt.Sprintf(format, a...)}
}
