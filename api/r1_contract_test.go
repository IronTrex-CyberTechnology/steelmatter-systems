package api

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/engine"
)

// R1-API-01/05/10: the R1 response schemas list exactly the fields the
// engine returns, required exactly when always present, and the recovery
// operations document exactly the error statuses they can produce.

func jsonFields(t *testing.T, v any) (all, always []string) {
	t.Helper()
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		all = append(all, name)
		if opts != "omitempty" {
			always = append(always, name)
		}
	}
	sort.Strings(all)
	sort.Strings(always)
	return all, always
}

func TestR1ResponseSchemasMatchEngineTypes(t *testing.T) {
	d := load(t)
	for name, v := range map[string]any{
		"DispatchResult":  engine.DispatchResult{},
		"RestoreReport":   engine.RestoreReport{},
		"ReconcileReport": engine.ReconcileReport{},
		"GateReport":      engine.GateReport{},
		"GateCheck":       engine.GateCheck{},
		"RestoreSignal":   engine.RestoreSignal{},
		"WriteState":      anchor.WriteState{},
	} {
		all, always := jsonFields(t, v)
		var props, required []string
		for k := range get(t, d, "components", "schemas", name, "properties").(doc) {
			props = append(props, k)
		}
		for _, r := range get(t, d, "components", "schemas", name, "required").([]any) {
			required = append(required, r.(string))
		}
		sort.Strings(props)
		sort.Strings(required)
		if !reflect.DeepEqual(props, all) {
			t.Errorf("%s: schema properties %v, engine fields %v", name, props, all)
		}
		if !reflect.DeepEqual(required, always) {
			t.Errorf("%s: schema required %v, always-present engine fields %v", name, required, always)
		}
	}
}

func TestR1RecoveryOperationsDocumentOnlyProducedErrors(t *testing.T) {
	d := load(t)
	want := map[string][]string{
		// No 409: concurrent calls wait for each other instead of conflicting.
		"/v1/recovery/reconcile": {"400", "401", "403", "429", "500", "503"},
		"/v1/recovery/resume":    {"400", "401", "403", "429", "500", "503"},
		"/v1/recovery/check":     {"400", "401", "403", "429", "500", "503"},
	}
	for path, codes := range want {
		var got []string
		for code := range get(t, d, "paths", path, "post", "responses").(doc) {
			if code >= "400" {
				got = append(got, code)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, codes) {
			t.Errorf("%s: documented errors %v, want %v", path, got, codes)
		}
	}
	desc := get(t, d, "paths", "/v1/dispatches", "post", "description").(string)
	if strings.Contains(desc, "403 while writes are off") || !strings.Contains(desc, "503 SERVICE_UNAVAILABLE") {
		t.Errorf("dispatch description must say writes-off is 503: %q", desc)
	}
}
