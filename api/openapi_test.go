// Package api holds the closed OpenAPI contract (I02 checklist item 2). These
// tests keep openapi.yaml closed, complete and consistent with the Go error
// catalogue and payload limits.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/api"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/httpapi"
)

type doc = map[string]any

func load(t *testing.T) doc {
	t.Helper()
	b, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func get(t *testing.T, d any, path ...string) any {
	t.Helper()
	cur := d
	for _, p := range path {
		m, ok := cur.(doc)
		if !ok {
			t.Fatalf("%v: not an object at %q", path, p)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("%v: missing %q", path, p)
		}
	}
	return cur
}

func enum(t *testing.T, d doc, name string) []string {
	t.Helper()
	var out []string
	for _, v := range get(t, d, "components", "schemas", name, "enum").([]any) {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

// Document 03 endpoint list plus the R1 additions, exactly.
var wantOperations = map[string]string{
	"POST /v1/mandates":                                "createMandate",
	"POST /v1/mandates/{mandate_id}/revoke":            "revokeMandate",
	"POST /v1/instructions":                            "createInstruction",
	"POST /v1/instructions/{instruction_id}/revisions": "amendInstruction",
	"POST /v1/actions/evaluate":                        "evaluateAction",
	"POST /v1/actions/simulate":                        "simulateAction",
	"POST /v1/operations":                              "admitOperation",
	"GET /v1/operations/{operation_id}":                "getOperation",
	"POST /v1/operations/{operation_id}/reconcile":     "reconcileOperation",
	"GET /v1/operations/{operation_id}/evidence":       "getEvidence",
	"GET /v1/adapters/{adapter_id}/assurance":          "getAdapterAssurance",
	// R1 additions (dispatch to a real destination, repairs, recovery).
	"POST /v1/dispatches":                                      "dispatchOperation",
	"GET /v1/dispatches/{op_ref}":                              "getDispatch",
	"POST /v1/dispatches/{op_ref}/settle":                      "settleDispatch",
	"POST /v1/dispatches/{op_ref}/findings":                    "recordHeldFinding",
	"POST /v1/dispatches/{op_ref}/repairs":                     "prepareRepair",
	"POST /v1/dispatches/{op_ref}/repairs/{repair_id}/review":  "reviewRepair",
	"POST /v1/dispatches/{op_ref}/repairs/{repair_id}/confirm": "confirmRepair",
	"POST /v1/dispatches/{op_ref}/anchor-repair":               "confirmAnchorRepair",
	"GET /v1/recovery/write-state":                             "getWriteState",
	"POST /v1/recovery/check":                                  "checkRestore",
	"POST /v1/recovery/reconcile":                              "reconcileRestore",
	"POST /v1/recovery/resume":                                 "resumeWrites",
	"POST /v1/instructions/{instruction_id}/approvals":         "recordApproval",
}

var principalKinds = map[string]bool{"owner": true, "issuer": true, "actor": true, "observer": true, "operator": true}

func TestOpenAPIIsVersion31(t *testing.T) {
	if v := get(t, load(t), "openapi").(string); !strings.HasPrefix(v, "3.1.") {
		t.Fatalf("openapi %s", v)
	}
}

func TestOpenAPIEndpointsMatchDocument03(t *testing.T) {
	d := load(t)
	got := map[string]string{}
	for path, item := range get(t, d, "paths").(doc) {
		for method, op := range item.(doc) {
			got[strings.ToUpper(method)+" "+path] = op.(doc)["operationId"].(string)
		}
	}
	if !reflect.DeepEqual(got, wantOperations) {
		t.Fatalf("operations differ:\n got %v\nwant %v", got, wantOperations)
	}
}

func TestOpenAPIOperationsDeclarePrincipalsAndErrors(t *testing.T) {
	d := load(t)
	for path, item := range get(t, d, "paths").(doc) {
		for method, raw := range item.(doc) {
			op := raw.(doc)
			name := strings.ToUpper(method) + " " + path
			ps, _ := op["x-steelmatter-principals"].([]any)
			if len(ps) == 0 {
				t.Errorf("%s: no x-steelmatter-principals", name)
			}
			for _, p := range ps {
				if !principalKinds[p.(string)] {
					t.Errorf("%s: unknown principal kind %v", name, p)
				}
			}
			responses := op["responses"].(doc)
			for _, code := range []string{"401", "403", "429", "503"} {
				if _, ok := responses[code]; !ok {
					t.Errorf("%s: missing %s response", name, code)
				}
			}
			if _, hasBody := op["requestBody"]; hasBody {
				if _, ok := responses["400"]; !ok {
					t.Errorf("%s: request body but no 400 response", name)
				}
			}
			for code, r := range responses {
				n, _ := strconv.Atoi(code)
				if n >= 400 {
					ref, _ := r.(doc)["$ref"].(string)
					if !strings.HasPrefix(ref, "#/components/responses/") {
						t.Errorf("%s %s: error response must reference a shared Error response", name, code)
					}
				}
			}
		}
	}
	for name, r := range get(t, d, "components", "responses").(doc) {
		ref := get(t, r, "content", "application/json", "schema", "$ref")
		if ref != "#/components/schemas/Error" {
			t.Errorf("response %s does not use the Error schema", name)
		}
	}
}

func TestOpenAPIErrorCodesEqualCatalogue(t *testing.T) {
	d := load(t)
	if got, want := enum(t, d, "ErrorCode"), httpapi.ErrorCodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ErrorCode enum differs from core/httpapi catalogue:\n got %v\nwant %v", got, want)
	}
	if got, want := enum(t, d, "DenyReason"), httpapi.DenyReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DenyReason enum differs from catalogue:\n got %v\nwant %v", got, want)
	}
}

func TestCatalogueCoversEveryContractCode(t *testing.T) {
	codes := []contract.Code{
		contract.CodePayloadTooLarge, contract.CodeInvalidEncoding, contract.CodeInvalidJSON, contract.CodeDuplicateKey,
		contract.CodeNonIntegerNumber, contract.CodeNumberOutOfRange, contract.CodeUnsupportedVersion, contract.CodeUnsupportedProfile,
		contract.CodeUnknownField, contract.CodeUnsupportedCurrency, contract.CodeInvalidTimestamp, contract.CodeSchemaViolation,
		contract.CodeDuplicateLineItem, contract.CodeLinesNotCanonical, contract.CodeArithmeticOverflow, contract.CodeAmountMismatch,
		contract.CodeInvalidTimeRange, contract.CodeObservationInconsistent,
		contract.CodeJWSMalformed, contract.CodeJWSAlgUnsupported, contract.CodeJWSHeaderUnsupported, contract.CodeJWSTypeMismatch,
		contract.CodeJWSKeyUntrusted, contract.CodeJWSSignatureInvalid, contract.CodeJWSPayloadNotCanonical,
	}
	for _, c := range codes {
		spec, ok := httpapi.Lookup(string(c))
		if !ok || spec.Use != httpapi.UseError {
			t.Errorf("contract code %s missing from the catalogue or not an error", c)
		}
	}
	seen := map[string]bool{}
	for _, c := range httpapi.Catalogue {
		if seen[c.Code] {
			t.Errorf("duplicate catalogue code %s", c.Code)
		}
		seen[c.Code] = true
		if c.Use == httpapi.UseDeny && c.Status != 200 {
			t.Errorf("deny-only code %s must be carried in a 200 DENY", c.Code)
		}
		if c.Use != httpapi.UseDeny && c.Status < 400 {
			t.Errorf("error code %s has non-error status %d", c.Code, c.Status)
		}
		if c.Retryable != (c.Status == 429 || c.Status == 503) {
			t.Errorf("code %s: retryable must be true exactly for 429/503", c.Code)
		}
	}
}

// Every object schema anywhere in the document must be closed.
func TestOpenAPISchemasAreClosed(t *testing.T) {
	d := load(t)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch n := v.(type) {
		case doc:
			_, hasProps := n["properties"]
			if n["type"] == "object" || hasProps {
				if ap, ok := n["additionalProperties"]; !ok || ap != false {
					t.Errorf("%s: object schema is not closed (additionalProperties: false)", path)
				}
			}
			for k, c := range n {
				if k == "examples" || k == "example" {
					continue
				}
				walk(path+"/"+k, c)
			}
		case []any:
			for i, c := range n {
				walk(fmt.Sprintf("%s/%d", path, i), c)
			}
		}
	}
	walk("#", d)
}

func TestOpenAPIReferencesResolve(t *testing.T) {
	d := load(t)
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case doc:
			if ref, ok := n["$ref"].(string); ok {
				if strings.HasPrefix(ref, "#/") {
					get(t, d, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
				} else if _, err := os.Stat(filepath.Clean(ref)); err != nil {
					t.Errorf("external $ref %s: %v", ref, err)
				}
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(d)
}

func TestOpenAPILimitsMatchContract(t *testing.T) {
	d := load(t)
	if got := get(t, d, "components", "schemas", "CompactJWS", "maxLength").(int); got != contract.MaxJWSBytes {
		t.Fatalf("CompactJWS maxLength %d; contract.MaxJWSBytes %d", got, contract.MaxJWSBytes)
	}
	if got := get(t, d, "components", "schemas", "PositiveInteger", "maximum").(int); int64(got) != contract.MaxSafeInteger {
		t.Fatalf("PositiveInteger maximum %d", got)
	}
}

// Request bodies must not accept server-owned identity fields.
func TestOpenAPIRequestsExcludeServerOwnedFields(t *testing.T) {
	d := load(t)
	forbidden := []string{"tenant_id", "actor_id", "issuer_id", "audience", "action_revision", "amount_minor", "supplier_revision", "target_url", "destination"}
	for _, name := range []string{"MandateCreateRequest", "InstructionCreateRequest", "AmendmentRequest", "ProposedAction",
		"FindingRequest", "RepairRequest", "ReviewRequest", "AnchorRepairRequest", "EmptyRequest"} {
		props := get(t, d, "components", "schemas", name, "properties").(doc)
		for _, f := range forbidden {
			if name == "MandateCreateRequest" && f == "audience" {
				continue // the owner sets the mandate's destination audience
			}
			if _, ok := props[f]; ok {
				t.Errorf("%s accepts server-owned field %s", name, f)
			}
		}
	}
}

// Every operation declared in openapi.yaml is routed by the service, so an
// endpoint cannot be declared but missing.
func TestOpenAPIEveryOperationIsRouted(t *testing.T) {
	mux := http.NewServeMux()
	api.Register(mux, nil, nil)
	for op := range wantOperations {
		method, path, _ := strings.Cut(op, " ")
		concrete := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "x-1")
		req := httptest.NewRequest(method, concrete, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("%s is declared in openapi.yaml but not routed", op)
		}
	}
}
