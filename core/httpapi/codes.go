package httpapi

import (
	"net/http"
	"sort"
)

// Use says where a code may appear.
type Use string

const (
	// UseError: only in the error envelope.
	UseError Use = "error"
	// UseDeny: only as the reason of a 200 DENY evaluation (policy denial).
	UseDeny Use = "deny"
	// UseBoth: a DENY reason at evaluation and an error at admission.
	UseBoth Use = "both"
)

// CodeSpec maps one condition to one stable code (I02 checklist item 2,
// document 03 Errors). api/openapi.yaml enumerates exactly these codes.
type CodeSpec struct {
	Code      string
	Status    int
	Retryable bool
	Use       Use
	Meaning   string
}

// Catalogue is the complete error/deny code list. Order is documentation
// order; Lookup is the access path.
var Catalogue = []CodeSpec{
	// Transport, identity and availability.
	{"UNAUTHENTICATED", http.StatusUnauthorized, false, UseError, "Missing or invalid credentials."},
	{"FORBIDDEN", http.StatusForbidden, false, UseError, "Authenticated principal lacks entitlement for this operation or scope. No mutation and no disclosure of whether the target exists."},
	{"NOT_FOUND", http.StatusNotFound, false, UseError, "Absent or inaccessible identity; indistinguishable by design."},
	{"RATE_LIMITED", http.StatusTooManyRequests, true, UseError, "Quota exceeded."},
	{"SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, true, UseError, "Authoritative database unavailable. No cache-only allow; retry keeps the same identities."},
	{"CLOCK_UNHEALTHY", http.StatusServiceUnavailable, true, UseError, "Clock-health check failed; admission and issuance are blocked."},
	{"INTERNAL_ERROR", http.StatusInternalServerError, false, UseError, "Unexpected server-side failure (for example a database constraint that should be unreachable). Logged with the request ID; not retryable without investigation."},
	{"RETRIES_EXHAUSTED", http.StatusServiceUnavailable, true, UseError, "Bounded database retries exhausted. operation_id is returned when one exists so the caller can look it up; never a new identity."},

	// Input profile (core/contract). 400 unless the semantics are unsupported (422).
	{"PAYLOAD_TOO_LARGE", http.StatusBadRequest, false, UseError, "Body exceeds 1 MiB."},
	{"INVALID_ENCODING", http.StatusBadRequest, false, UseError, "Not UTF-8, byte-order mark, or unpaired surrogate escape."},
	{"INVALID_JSON", http.StatusBadRequest, false, UseError, "JSON syntax error, trailing content or excessive depth."},
	{"DUPLICATE_KEY", http.StatusBadRequest, false, UseError, "An object repeats a key."},
	{"NON_INTEGER_NUMBER", http.StatusBadRequest, false, UseError, "Number with fraction, exponent or negative zero."},
	{"NUMBER_OUT_OF_RANGE", http.StatusBadRequest, false, UseError, "Integer outside ±(2^53-1)."},
	{"UNKNOWN_FIELD", http.StatusBadRequest, false, UseError, "Field not in the closed schema, including server-owned identity fields."},
	{"INVALID_TIMESTAMP", http.StatusBadRequest, false, UseError, "Not a real UTC YYYY-MM-DDTHH:MM:SSZ time."},
	{"SCHEMA_VIOLATION", http.StatusBadRequest, false, UseError, "Violates the JSON Schema (type, pattern, bounds, required)."},
	{"DUPLICATE_LINE_ITEM", http.StatusBadRequest, false, UseError, "Same item_id on more than one line; never merged."},
	{"LINES_NOT_CANONICAL", http.StatusBadRequest, false, UseError, "Stored or signed lines not sorted by item_id."},
	{"ARITHMETIC_OVERFLOW", http.StatusBadRequest, false, UseError, "Line product or total exceeds 2^53-1."},
	{"AMOUNT_MISMATCH", http.StatusBadRequest, false, UseError, "amount_minor differs from the line total."},
	{"INVALID_TIME_RANGE", http.StatusBadRequest, false, UseError, "expires_at not after issued_at, or permit lifetime above 5 minutes."},
	{"OBSERVATION_INCONSISTENT", http.StatusBadRequest, false, UseError, "Observation status disagrees with its digests."},
	{"UNSUPPORTED_VERSION", http.StatusUnprocessableEntity, false, UseError, "schema_version is not \"1\"."},
	{"UNSUPPORTED_PROFILE", http.StatusUnprocessableEntity, false, UseError, "crypto_profile is not SM-JWS-ES256-1."},
	{"UNSUPPORTED_CURRENCY", http.StatusUnprocessableEntity, false, UseError, "Currency other than INR."},
	{"ADAPTER_UNSUPPORTED", http.StatusUnprocessableEntity, false, UseError, "Operation or adapter path not supported by this destination."},
	{"SUPPLIER_UNAVAILABLE", http.StatusUnprocessableEntity, false, UseError, "Proposed supplier is unknown or suspended in the protected supplier records."},
	{"ACTOR_ASSIGNMENT_INVALID", http.StatusUnprocessableEntity, false, UseError, "Source-workflow actor assignment missing, ambiguous, disabled or outside the mandate. Nothing is created."},

	// Presented permit (JWS profile).
	{"JWS_MALFORMED", http.StatusBadRequest, false, UseError, "Permit is not a pinned-form compact JWS."},
	{"JWS_ALG_UNSUPPORTED", http.StatusBadRequest, false, UseError, "alg is not ES256 (including none)."},
	{"JWS_HEADER_UNSUPPORTED", http.StatusBadRequest, false, UseError, "Header member other than alg, kid, typ (crit, jku, jwk, x5u, ...)."},
	{"JWS_TYPE_MISMATCH", http.StatusBadRequest, false, UseError, "typ is not the expected payload type."},
	{"JWS_PAYLOAD_NOT_CANONICAL", http.StatusBadRequest, false, UseError, "Signed payload is not RFC 8785 canonical."},
	{"JWS_KEY_UNTRUSTED", http.StatusForbidden, false, UseError, "kid absent from the local trust store or not allowed for this typ."},
	{"JWS_SIGNATURE_INVALID", http.StatusForbidden, false, UseError, "Signature does not verify (tampering, wrong key, malformed signature)."},

	// Authority. Denials at evaluation; errors at admission.
	{"AUTHORITY_REVOKED", http.StatusForbidden, false, UseBoth, "Mandate revoked or control epoch changed."},
	{"MANDATE_EXPIRED", http.StatusForbidden, false, UseBoth, "Mandate expired (exclusive expiry, checked after locks)."},
	{"ACTOR_MISMATCH", http.StatusForbidden, false, UseError, "Authenticated caller is not the instruction's assigned actor, the trusted assignment no longer selects them, or the presented action names another actor."},
	{"AUDIENCE_MISMATCH", http.StatusForbidden, false, UseError, "Permit or action audience is not this destination."},
	{"PERMIT_EXPIRED", http.StatusForbidden, false, UseError, "Permit expired, rechecked after lock acquisition."},
	{"SUPPLIER_NOT_PERMITTED", http.StatusOK, false, UseDeny, "Supplier outside the mandate."},
	{"DELIVERY_NOT_PERMITTED", http.StatusOK, false, UseDeny, "Delivery point outside the mandate."},
	{"CAPACITY_EXHAUSTED", http.StatusOK, false, UseDeny, "Shared allowance insufficient for the amount."},
	{"APPROVAL_REQUIRED", http.StatusOK, false, UseDeny, "Above the tenant's approval threshold, the exact action needs an approval by an entitled approver (T17-T19)."},
	{"APPROVAL_STALE", http.StatusOK, false, UseDeny, "The approval no longer matches the action, supplier or mandate, or its approver lost approval rights; a fresh decision is required (T17, T18)."},

	// State conflicts.
	{"SOURCE_CONFLICT", http.StatusConflict, false, UseError, "Trusted source record already exists with a different creation request."},
	{"ACTION_CHANGED", http.StatusConflict, false, UseError, "Presented action digest differs from the permitted revision (changed replay)."},
	{"REVISION_STALE", http.StatusConflict, false, UseError, "Action revision is no longer current, or expected_action_revision is outdated."},
	{"AMENDMENT_ID_CONFLICT", http.StatusConflict, false, UseError, "amendment_id reused with a different request."},
	{"ALREADY_COMMITTED", http.StatusConflict, false, UseError, "Instruction has a committed operation; it cannot be amended."},
	{"SUPPLIER_REVISION_STALE", http.StatusConflict, false, UseError, "Protected supplier revision changed after authorisation; re-evaluation required."},
	{"DELIVERY_REVISION_STALE", http.StatusConflict, false, UseError, "Protected delivery address revision changed after the action was built; amend and re-evaluate."},
	{"PERMIT_NOT_ACTIVE", http.StatusConflict, false, UseError, "Permit was released, invalidated or consumed by a different action."},
}

var byCode = func() map[string]CodeSpec {
	m := map[string]CodeSpec{}
	for _, c := range Catalogue {
		m[c.Code] = c
	}
	return m
}()

// Lookup returns the spec for code.
func Lookup(code string) (CodeSpec, bool) {
	c, ok := byCode[code]
	return c, ok
}

// ErrorCodes returns the sorted codes allowed in the error envelope.
func ErrorCodes() []string { return codesFor(UseError, UseBoth) }

// DenyReasons returns the sorted codes allowed as DENY reasons.
func DenyReasons() []string { return codesFor(UseDeny, UseBoth) }

func codesFor(uses ...Use) []string {
	var out []string
	for _, c := range Catalogue {
		for _, u := range uses {
			if c.Use == u {
				out = append(out, c.Code)
			}
		}
	}
	sort.Strings(out)
	return out
}
