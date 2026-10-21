//go:build conformance

// Package conformance checks the Go engine against the cross-language golden
// vectors and against the TypeScript verifier (I02; contract portions of
// T20, T21, T39, T40, T41). These are contract checks: they do not mark any
// whole acceptance case PASS.
package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/contract"
	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
)

const root = "../.."

type vectors struct {
	VectorSet  string          `json:"vector_set"`
	TrustStore json.RawMessage `json:"trust_store"`
	Canonical  []struct {
		Name          string        `json:"name"`
		Kind          contract.Kind `json:"kind"`
		InputB64      string        `json:"input_b64"`
		Canonical     string        `json:"canonical"`
		DigestContext string        `json:"digest_context"`
		Digest        string        `json:"digest"`
	} `json:"canonical"`
	PayloadNegative []struct {
		Name         string        `json:"name"`
		Kind         contract.Kind `json:"kind"`
		InputB64     string        `json:"input_b64"`
		ExpectedCode contract.Code `json:"expected_code"`
	} `json:"payload_negative"`
	JWSPositive []struct {
		Name, Kid, JWS  string
		Kind            contract.Kind
		ProtectedHeader string `json:"protected_header"`
		SigningInput    string `json:"signing_input"`
		PayloadDigest   string `json:"payload_digest"`
	} `json:"jws_positive"`
	JWSNegative []struct {
		Name, JWS    string
		Kind         contract.Kind
		ExpectedCode contract.Code `json:"expected_code"`
	} `json:"jws_negative"`
}

func load(t *testing.T) (*vectors, *keys.TrustStore) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "fixtures/vectors/golden-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.VectorSet != "steelmatter-golden-v1" || len(v.Canonical) == 0 || len(v.PayloadNegative) == 0 || len(v.JWSPositive) == 0 || len(v.JWSNegative) == 0 {
		t.Fatalf("vector file incomplete: %s", v.VectorSet)
	}
	ts, err := keys.ParseTrustStore(v.TrustStore)
	if err != nil {
		t.Fatal(err)
	}
	return &v, ts
}

func dec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformanceCanonicalVectors(t *testing.T) {
	v, _ := load(t)
	for _, c := range v.Canonical {
		t.Run(c.Name, func(t *testing.T) {
			in := dec(t, c.InputB64)
			canonical, digest, err := contract.CanonicalDigest(c.Kind, in)
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != c.Canonical || digest != c.Digest || contract.DigestContext(c.Kind) != c.DigestContext {
				t.Fatalf("canonical/digest mismatch:\n got %s %s\nwant %s %s", canonical, digest, c.Canonical, c.Digest)
			}
		})
	}
}

func TestConformancePayloadNegativeVectors(t *testing.T) {
	v, _ := load(t)
	for _, c := range v.PayloadNegative {
		t.Run(c.Name, func(t *testing.T) {
			_, err := contract.Validate(c.Kind, dec(t, c.InputB64))
			if err == nil || err.Code != c.ExpectedCode {
				t.Fatalf("got %v; want %s", err, c.ExpectedCode)
			}
		})
	}
}

func TestConformanceJWSPositiveVectors(t *testing.T) {
	v, trust := load(t)
	for _, c := range v.JWSPositive {
		t.Run(c.Name, func(t *testing.T) {
			h, err := contract.ProtectedHeader(c.Kind, c.Kid)
			if err != nil || string(h) != c.ProtectedHeader {
				t.Fatalf("protected header %s; want %s", h, c.ProtectedHeader)
			}
			payload := dec(t, strings.Split(c.JWS, ".")[1])
			si, _ := contract.SigningInput(c.Kind, c.Kid, payload)
			if si != c.SigningInput {
				t.Fatal("signing input mismatch")
			}
			res, verr := contract.Verify(c.JWS, c.Kind, trust)
			if verr != nil {
				t.Fatal(verr)
			}
			if res.Digest != c.PayloadDigest {
				t.Fatalf("digest %s; want %s", res.Digest, c.PayloadDigest)
			}
		})
	}
}

func TestConformanceJWSNegativeVectors(t *testing.T) {
	v, trust := load(t)
	for _, c := range v.JWSNegative {
		t.Run(c.Name, func(t *testing.T) {
			_, err := contract.Verify(c.JWS, c.Kind, trust)
			if err == nil || err.Code != c.ExpectedCode {
				t.Fatalf("got %v; want %s", err, c.ExpectedCode)
			}
		})
	}
}

// The packet's own negative-cases.json (schema shape checks plus the one
// semantic gap: duplicate item IDs pass the schema but must be rejected).
func TestConformancePacketNegativeCases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "fixtures/packet/negative-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name, Expected string
			PayloadType    contract.Kind   `json:"payload_type"`
			Payload        json.RawMessage `json:"payload"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := contract.Validate(c.PayloadType, c.Payload); err == nil {
				t.Fatalf("accepted; packet expects %s and the application must reject", c.Expected)
			}
		})
	}
}

func TestConformanceVerifierSchemasMatchEngine(t *testing.T) {
	for _, k := range contract.Kinds {
		name := string(k) + ".schema.json"
		a, err := os.ReadFile(filepath.Join(root, "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(root, "packages/verifier/schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs between schemas/ and packages/verifier/schemas/", name)
		}
	}
}

// Fresh Go signatures verify in TypeScript and fresh TypeScript signatures
// verify in Go; signing inputs agree; signature bytes are not compared.
func TestConformanceCrossRuntimeFreshSignatures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for cross-runtime conformance")
	}
	dir := t.TempDir()
	purposes := []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"}
	entry, err := keys.Provision(dir, "go-fresh", purposes, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	priv, err := keys.ReadPrivateKey(filepath.Join(dir, "go-fresh.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	goTrust, _ := os.ReadFile(filepath.Join(dir, "trust-store.json"))

	type signed struct {
		Kind         contract.Kind `json:"kind"`
		JWS          string        `json:"jws"`
		SigningInput string        `json:"signing_input,omitempty"`
	}
	in := struct {
		TrustStore json.RawMessage `json:"trust_store"`
		Signed     []signed        `json:"signed"`
	}{TrustStore: goTrust}
	signedKinds := []contract.Kind{contract.KindMandate, contract.KindPermit, contract.KindExecution, contract.KindObservation}
	for _, k := range signedKinds {
		raw, err := os.ReadFile(filepath.Join(root, "fixtures/packet", string(k)+".payload.json"))
		if err != nil {
			t.Fatal(err)
		}
		jws, err := contract.Sign(k, raw, contract.Signer{Kid: entry.Kid, Key: priv})
		if err != nil {
			t.Fatal(err)
		}
		in.Signed = append(in.Signed, signed{Kind: k, JWS: jws})
	}
	inPath, outPath := filepath.Join(dir, "go.json"), filepath.Join(dir, "ts.json")
	b, _ := json.Marshal(in)
	if err := os.WriteFile(inPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "src/cli.ts", "cross", inPath, outPath)
	cmd.Dir = filepath.Join(root, "packages/verifier")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TypeScript failed to verify Go signatures: %v\n%s", err, out)
	}

	var res struct {
		TrustStore json.RawMessage `json:"trust_store"`
		VerifiedGo []struct {
			Kind   contract.Kind `json:"kind"`
			Digest string        `json:"digest"`
		} `json:"verified_go"`
		Signed []signed `json:"signed"`
	}
	ob, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ob, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.VerifiedGo) != len(signedKinds) || len(res.Signed) != len(signedKinds) {
		t.Fatalf("TypeScript verified %d and signed %d of %d", len(res.VerifiedGo), len(res.Signed), len(signedKinds))
	}
	tsTrust, err := keys.ParseTrustStore(res.TrustStore)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range res.Signed {
		v, verr := contract.Verify(s.JWS, s.Kind, tsTrust)
		if verr != nil {
			t.Fatalf("Go failed to verify TypeScript %s signature: %v", s.Kind, verr)
		}
		si, _ := contract.SigningInput(s.Kind, tsTrust.Keys[0].Kid, v.Payload)
		if si != s.SigningInput || !strings.HasPrefix(s.JWS, si+".") {
			t.Fatalf("%s signing input differs between runtimes", s.Kind)
		}
		if v.Digest != res.VerifiedGo[i].Digest {
			t.Fatalf("%s digest differs: Go %s, TypeScript %s", s.Kind, v.Digest, res.VerifiedGo[i].Digest)
		}
		if s.JWS == in.Signed[i].JWS {
			t.Fatal("identical fresh signatures across runtimes; randomness suspect")
		}
	}
}

// QA-D1-002: Go and TypeScript trust-store parsers agree on every case in
// fixtures/vectors/truststore-v1.json (the TypeScript half runs in
// packages/verifier/test/truststore.test.ts).
func TestConformanceTrustStoreParsing(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "fixtures/vectors/truststore-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name   string `json:"name"`
			Expect string `json:"expect"`
			Store  string `json:"store"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := keys.ParseTrustStore([]byte(c.Store))
			if got := map[bool]string{true: "accept", false: "reject"}[err == nil]; got != c.Expect {
				t.Fatalf("Go %s (%v); want %s", got, err, c.Expect)
			}
		})
	}
}
