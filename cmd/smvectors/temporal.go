package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
)

// ---- cross-record timestamp fixture (ADR-003) ----

type temporalCase struct {
	Name string `json:"name"`
	// Covers names the reported scenario this case reproduces ("" for extra cases).
	Covers string         `json:"covers"`
	Valid  bool           `json:"valid"`
	Bundle map[string]any `json:"bundle"`
	Expect temporalExpect `json:"expect"`
}

type temporalExpect struct {
	Linkage      string   `json:"linkage"`
	Chronology   string   `json:"chronology"`
	BrokenChecks []string `json:"broken_checks"`
}

type temporalFile struct {
	FixtureSet string           `json:"fixture_set"`
	Note       string           `json:"note"`
	TrustStore *keys.TrustStore `json:"trust_store"`
	Cases      []temporalCase   `json:"cases"`
}

// times are offsets in seconds from the packet mandate's issued_at.
type times struct{ mI, mX, pI, pX, eC, oO int }

// runTemporal builds validly signed, correctly digest-linked chains from the
// packet examples whose only differences are their timestamps, and checks
// every expectation against contract.TemporalLinkage before writing.
func runTemporal(out, packetDir string) error {
	signer, err := newKey("temporal-fixture-signer", []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"})
	if err != nil {
		return err
	}
	load := func(k contract.Kind) (map[string]any, error) {
		b, err := os.ReadFile(filepath.Join(packetDir, string(k)+".payload.json"))
		if err != nil {
			return nil, err
		}
		return decode(b)
	}
	base := map[contract.Kind]map[string]any{}
	for _, k := range []contract.Kind{contract.KindMandate, contract.KindAction, contract.KindPermit, contract.KindExecution, contract.KindObservation} {
		if base[k], err = load(k); err != nil {
			return err
		}
	}
	t0, err := contract.ParseTimestamp(base[contract.KindMandate]["issued_at"].(string))
	if err != nil {
		return err
	}
	ts := func(s int) string { return t0.Add(time.Duration(s) * time.Second).UTC().Format("2006-01-02T15:04:05Z") }
	dig := func(k contract.Kind, m map[string]any) string { return contract.Digest(k, []byte(canonicalJSON(m))) }
	sign := func(k contract.Kind, m map[string]any) (string, error) {
		return contract.Sign(k, []byte(canonicalJSON(m)), contract.Signer{Kid: signer.entry.Kid, Key: signer.priv})
	}
	action := base[contract.KindAction]
	actionDigest := dig(contract.KindAction, action)

	type built struct {
		bundle map[string]any
		m      *contract.Mandate
		p      *contract.Permit
		e      *contract.Execution
		o      *contract.Observation
	}
	build := func(t times, withExecution bool) (*built, error) {
		mandate, permit, execution, observation := clone(base[contract.KindMandate]), clone(base[contract.KindPermit]), clone(base[contract.KindExecution]), clone(base[contract.KindObservation])
		mandate["issued_at"], mandate["expires_at"] = ts(t.mI), ts(t.mX)
		permit["issued_at"], permit["expires_at"] = ts(t.pI), ts(t.pX)
		permit["action_digest"], permit["mandate_digest"] = actionDigest, dig(contract.KindMandate, mandate)
		execution["committed_at"] = ts(t.eC)
		execution["action_digest"], execution["permit_digest"] = actionDigest, dig(contract.KindPermit, permit)
		observation["observed_at"] = ts(t.oO)
		observation["execution_digest"] = dig(contract.KindExecution, execution)
		observation["expected_action_digest"], observation["observed_action_digest"] = actionDigest, actionDigest
		b := &built{}
		recs := map[string]any{}
		for _, r := range []struct {
			k contract.Kind
			m map[string]any
		}{{contract.KindMandate, mandate}, {contract.KindPermit, permit}, {contract.KindExecution, execution}, {contract.KindObservation, observation}} {
			if !withExecution && (r.k == contract.KindExecution || r.k == contract.KindObservation) {
				continue
			}
			jws, err := sign(r.k, r.m)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.k, err)
			}
			recs[string(r.k)+"_jws"] = jws
			v, verr := contract.Verify(jws, r.k, &keys.TrustStore{Version: keys.TrustStoreVer, Keys: []keys.TrustedKey{signer.entry}})
			if verr != nil {
				return nil, fmt.Errorf("%s does not verify: %v", r.k, verr)
			}
			switch p := v.Value.(type) {
			case *contract.Mandate:
				b.m = p
			case *contract.Permit:
				b.p = p
			case *contract.Execution:
				b.e = p
			case *contract.Observation:
				b.o = p
			}
		}
		links := map[string]any{"instruction_id": action["instruction_id"], "action_digest": actionDigest,
			"mandate_digest": permit["mandate_digest"], "permit_digest": dig(contract.KindPermit, permit)}
		evidence, incomplete := "AVAILABLE", []any{}
		if withExecution {
			links["execution_digest"], links["obligation_id"] = dig(contract.KindExecution, execution), execution["obligation_id"]
		} else {
			evidence, incomplete = "PENDING", []any{"execution_jws", "observation_jws"}
		}
		b.bundle = map[string]any{"operation_id": permit["operation_id"], "evidence": evidence, "records": recs, "action": action,
			"links": links, "incomplete": incomplete}
		return b, nil
	}

	// The packet chain: mandate [0, 3600), permit [60, 360), commit 120, observed 180.
	ok := times{0, 3600, 60, 360, 120, 180}
	with := func(f func(*times)) times { t := ok; f(&t); return t }
	specs := []struct {
		name, covers string
		t            times
		exec         bool
		linkage      string
		broken       []string
	}{
		// Valid controls.
		{"consistent", "valid control", ok, true, "COMPLETE", nil},
		{"observation-same-second-as-execution", "valid control: observation and execution in the same recorded second", with(func(t *times) { t.oO = 120 }), true, "COMPLETE", nil},
		{"execution-last-second-before-permit-expiry", "valid control: authorised before the permit expired", with(func(t *times) { t.eC, t.oO = 359, 359 }), true, "COMPLETE", nil},
		{"boundaries-equal", "valid control: every inclusive bound equal", times{0, 300, 0, 300, 0, 0}, true, "COMPLETE", nil},
		{"pending-without-execution", "valid control: pending chain (no execution yet)", ok, false, "INCOMPLETE", nil},
		// The six reported scenarios.
		{"execution-before-permit-issuance", "S1 execution before permit issuance", with(func(t *times) { t.eC = 30 }), true, "BROKEN", []string{"time:execution_within_permit"}},
		{"execution-at-permit-expiry-boundary", "S2 execution exactly at the exclusive expiry boundary", with(func(t *times) { t.eC, t.oO = 360, 400 }), true, "BROKEN", []string{"time:execution_within_permit"}},
		{"execution-after-permit-expiry", "S3 execution after permit expiry", with(func(t *times) { t.eC, t.oO = 400, 500 }), true, "BROKEN", []string{"time:execution_within_permit"}},
		{"observation-before-execution", "S4 observation before execution", with(func(t *times) { t.oO = 90 }), true, "BROKEN", []string{"time:observation_after_execution"}},
		{"permit-issued-before-mandate", "S5 permit issuance before mandate issuance", with(func(t *times) { t.mI = 90 }), true, "BROKEN", []string{"time:permit_issued_within_mandate"}},
		{"permit-expiry-beyond-mandate-expiry", "S6 permit expiry extending beyond mandate expiry", with(func(t *times) { t.mX = 200 }), true, "BROKEN", []string{"time:permit_expiry_within_mandate"}},
		// Further combinations.
		{"execution-after-mandate-expiry", "", with(func(t *times) { t.mX, t.eC = 110, 115 }), true, "BROKEN",
			[]string{"time:permit_expiry_within_mandate", "time:execution_within_mandate"}},
		{"permit-issued-at-mandate-expiry", "", with(func(t *times) { t.mX = 60 }), true, "BROKEN",
			[]string{"time:permit_issued_within_mandate", "time:permit_expiry_within_mandate", "time:execution_within_mandate"}},
	}
	tf := temporalFile{
		FixtureSet: "steelmatter-evidence-temporal-v1",
		Note: "ADR-003 cross-record timestamp rules. Every record is validly signed and every digest link holds; the cases differ only in timestamps " +
			"(offsets from the packet mandate's issued_at). broken_checks lists exactly the linkage checks that must be MISMATCH; chronology is the verifier's chronology status. " +
			"covers maps the six reported scenarios (S1-S6) and the valid controls. The private key was discarded.",
		TrustStore: &keys.TrustStore{Version: keys.TrustStoreVer, Keys: []keys.TrustedKey{signer.entry}},
	}
	for _, s := range specs {
		b, err := build(s.t, s.exec)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		got := []string{}
		for _, c := range contract.TemporalLinkage(b.m, b.p, b.e, b.o) {
			if c.Status == contract.TemporalMismatch {
				got = append(got, c.Name)
			}
		}
		want := s.broken
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%s: Go rules report %v, fixture expects %v", s.name, got, want)
		}
		chronology := map[string]string{"COMPLETE": "CONSISTENT", "INCOMPLETE": "INCOMPLETE", "BROKEN": "INCONSISTENT"}[s.linkage]
		tf.Cases = append(tf.Cases, temporalCase{s.name, s.covers, len(want) == 0, b.bundle, temporalExpect{s.linkage, chronology, want}})
	}
	out2, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(out2, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d temporal cases (self-checked against the Go rules)\n", out, len(tf.Cases))
	return nil
}
