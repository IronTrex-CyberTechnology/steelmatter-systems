// Command smvectors generates the cross-language golden vectors
// (fixtures/vectors/golden-v1.json) from the packet example payloads.
//
//	go run ./cmd/smvectors -out fixtures/vectors/golden-v1.json
//
// Signing keys are generated fresh and discarded; only public keys, payloads,
// signatures and expected results are written. Every vector is checked against
// the Go implementation before the file is written, so the generator fails if
// the implementation and the intended expectation disagree. Regenerating the
// file changes keys and signatures, so treat it as a reviewed contract change.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
)

type canonicalVector struct {
	Name          string        `json:"name"`
	Kind          contract.Kind `json:"kind"`
	Input         string        `json:"input"`
	InputB64      string        `json:"input_b64"`
	Canonical     string        `json:"canonical"`
	DigestContext string        `json:"digest_context"`
	Digest        string        `json:"digest"`
}

type payloadNegative struct {
	Name         string        `json:"name"`
	Kind         contract.Kind `json:"kind"`
	Input        string        `json:"input,omitempty"`
	InputB64     string        `json:"input_b64"`
	ExpectedCode contract.Code `json:"expected_code"`
}

type jwsPositive struct {
	Name            string        `json:"name"`
	Kind            contract.Kind `json:"kind"`
	Kid             string        `json:"kid"`
	ProtectedHeader string        `json:"protected_header"`
	SigningInput    string        `json:"signing_input"`
	JWS             string        `json:"jws"`
	PayloadDigest   string        `json:"payload_digest"`
}

type jwsNegative struct {
	Name         string        `json:"name"`
	Kind         contract.Kind `json:"kind"`
	JWS          string        `json:"jws"`
	ExpectedCode contract.Code `json:"expected_code"`
}

type vectorFile struct {
	VectorSet       string            `json:"vector_set"`
	Baseline        string            `json:"baseline"`
	CryptoProfile   string            `json:"crypto_profile"`
	Note            string            `json:"note"`
	TrustStore      *keys.TrustStore  `json:"trust_store"`
	Canonical       []canonicalVector `json:"canonical"`
	PayloadNegative []payloadNegative `json:"payload_negative"`
	JWSPositive     []jwsPositive     `json:"jws_positive"`
	JWSNegative     []jwsNegative     `json:"jws_negative"`
}

var b64 = base64.RawURLEncoding

func main() {
	out := flag.String("out", "fixtures/vectors/golden-v1.json", "golden vector output file")
	evidenceOut := flag.String("evidence-out", "fixtures/vectors/evidence-v1.json", "evidence bundle fixture output file")
	trustOut := flag.String("truststore-out", "fixtures/vectors/truststore-v1.json", "trust-store parsing fixture output file")
	temporalOut := flag.String("temporal-out", "fixtures/vectors/evidence-temporal-v1.json", "cross-record timestamp fixture output file (ADR-003)")
	only := flag.String("only", "", "generate only one set: golden, evidence, truststore or temporal")
	packetDir := flag.String("packet", "fixtures/packet", "directory with <kind>.payload.json")
	flag.Parse()
	var err error
	switch *only {
	case "":
		if err = run(*out, *packetDir); err == nil {
			err = runEvidence(*evidenceOut, *packetDir)
		}
	case "golden":
		err = run(*out, *packetDir)
	case "evidence":
		err = runEvidence(*evidenceOut, *packetDir)
	case "truststore":
		err = runTrustStore(*trustOut)
	case "temporal":
		err = runTemporal(*temporalOut, *packetDir)
	default:
		err = fmt.Errorf("unknown -only %q", *only)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "smvectors:", err)
		os.Exit(1)
	}
}

type key struct {
	priv  *ecdsa.PrivateKey
	entry keys.TrustedKey
}

func newKey(name string, purposes []string) (key, error) {
	priv, err := keys.Generate()
	if err != nil {
		return key{}, err
	}
	jwk, err := keys.JWKFromPublic(&priv.PublicKey)
	if err != nil {
		return key{}, err
	}
	return key{priv: priv, entry: keys.TrustedKey{
		Kid: keys.Thumbprint(jwk), Name: name, Alg: keys.Algorithm, Synthetic: true, Purposes: purposes,
		CreatedAt: time.Now().UTC().Truncate(time.Second), JWK: jwk,
	}}, nil
}

func run(out, packetDir string) error {
	allPurposes := []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"}
	signer, err := newKey("golden-vector-signer", allPurposes)
	if err != nil {
		return err
	}
	observerOnly, err := newKey("golden-vector-observer-only", []string{"SM-OBSERVATION+JWS"})
	if err != nil {
		return err
	}
	attacker, err := newKey("untrusted-attacker", allPurposes) // not added to the trust store
	if err != nil {
		return err
	}
	trust := &keys.TrustStore{Version: keys.TrustStoreVer, Keys: []keys.TrustedKey{signer.entry, observerOnly.entry}}

	packets := map[contract.Kind]map[string]any{}
	for _, k := range contract.Kinds {
		b, err := os.ReadFile(filepath.Join(packetDir, string(k)+".payload.json"))
		if err != nil {
			return err
		}
		m, err := decode(b)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		packets[k] = m
	}

	vf := vectorFile{
		VectorSet:     "steelmatter-golden-v1",
		Baseline:      "0.6",
		CryptoProfile: contract.CryptoProfile,
		Note:          "Synthetic golden vectors for SM-JWS-ES256-1 (docs/contracts/I02-contract.md). Private keys were discarded after generation. Digest placeholders inside payloads come from the packet examples and are not linked records.",
		TrustStore:    trust,
	}

	// Canonical vectors: messy but valid input → canonical bytes and digest.
	for _, k := range contract.Kinds {
		input := messy(packets[k], "")
		addCanonical(&vf, "packet-"+string(k), k, input)
	}
	byteOrder := clone(packets[contract.KindAction])
	byteOrder["lines"] = []any{line("Item-Z", 1, 2), line("item-a", 3, 4)}
	byteOrder["amount_minor"] = json.Number("14")
	addCanonical(&vf, "action-byte-ordered-lines", contract.KindAction, messy(byteOrder, ""))
	bounds := clone(packets[contract.KindAction])
	bounds["lines"] = []any{line("item-a", 1, contract.MaxSafeInteger)}
	bounds["amount_minor"] = json.Number(fmt.Sprint(contract.MaxSafeInteger))
	bounds["action_revision"] = json.Number(fmt.Sprint(contract.MaxSafeInteger))
	addCanonical(&vf, "action-max-safe-integers", contract.KindAction, messy(bounds, ""))
	insufficient := clone(packets[contract.KindObservation])
	insufficient["status"] = "INSUFFICIENT_EVIDENCE"
	insufficient["observed_action_digest"] = nil
	addCanonical(&vf, "observation-insufficient-evidence-null-digest", contract.KindObservation, messy(insufficient, ""))

	addPayloadNegatives(&vf, packets)

	// JWS positives: each signed kind, plus an expired historical permit
	// (integrity verifies; usability is a separate check).
	for _, k := range []contract.Kind{contract.KindMandate, contract.KindPermit, contract.KindExecution, contract.KindObservation} {
		if err := addJWSPositive(&vf, "packet-"+string(k), k, canonicalJSON(packets[k]), signer); err != nil {
			return err
		}
	}

	if err := addJWSNegatives(&vf, packets, signer, observerOnly, attacker); err != nil {
		return err
	}

	// Self-check every vector against the Go implementation.
	if err := check(&vf); err != nil {
		return err
	}
	b, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d canonical, %d payload negative, %d JWS positive, %d JWS negative\n",
		out, len(vf.Canonical), len(vf.PayloadNegative), len(vf.JWSPositive), len(vf.JWSNegative))
	return nil
}

func addCanonical(vf *vectorFile, name string, k contract.Kind, input string) {
	canonical, err := contract.Canonicalize([]byte(input))
	if err != nil {
		panic(err)
	}
	vf.Canonical = append(vf.Canonical, canonicalVector{
		Name: name, Kind: k, Input: input, InputB64: b64.EncodeToString([]byte(input)),
		Canonical: string(canonical), DigestContext: contract.DigestContext(k), Digest: contract.Digest(k, canonical),
	})
}

func addPayloadNegatives(vf *vectorFile, packets map[contract.Kind]map[string]any) {
	add := func(name string, k contract.Kind, input []byte, code contract.Code) {
		n := payloadNegative{Name: name, Kind: k, InputB64: b64.EncodeToString(input), ExpectedCode: code}
		if isPrintableUTF8(input) {
			n.Input = string(input)
		}
		vf.PayloadNegative = append(vf.PayloadNegative, n)
	}
	mut := func(k contract.Kind, f func(m map[string]any)) []byte {
		m := clone(packets[k])
		f(m)
		return []byte(canonicalJSON(m))
	}
	A := canonicalJSON(packets[contract.KindAction])
	rep := func(old, new string) []byte {
		if !strings.Contains(A, old) {
			panic("fixture lacks " + old)
		}
		return []byte(strings.Replace(A, old, new, 1))
	}
	act, man, per, obs, exe := contract.KindAction, contract.KindMandate, contract.KindPermit, contract.KindObservation, contract.KindExecution

	// Encoding and syntax.
	add("byte-order-mark", act, append([]byte{0xEF, 0xBB, 0xBF}, A...), contract.CodeInvalidEncoding)
	add("invalid-utf8", act, rep(`"synthetic-org"`, "\"synthetic-\xff\""), contract.CodeInvalidEncoding)
	add("lone-surrogate-escape", act, rep(`"synthetic-org"`, `"synthetic-\ud800"`), contract.CodeInvalidEncoding)
	add("trailing-comma", act, []byte(strings.TrimSuffix(A, "}")+",}"), contract.CodeInvalidJSON)
	add("trailing-content", act, []byte(A+" {}"), contract.CodeInvalidJSON)
	add("leading-zero", act, rep(`"action_revision":1`, `"action_revision":01`), contract.CodeInvalidJSON)
	add("nan-literal", act, rep(`"amount_minor":60000000`, `"amount_minor":NaN`), contract.CodeInvalidJSON)
	add("duplicate-json-key", act, []byte(`{"amount_minor":60000000,`+A[1:]), contract.CodeDuplicateKey)
	add("duplicate-json-key-via-escape", act, []byte(`{"\u0061mount_minor":60000000,`+A[1:]), contract.CodeDuplicateKey)
	add("fractional-money", act, rep(`"amount_minor":60000000`, `"amount_minor":60000000.5`), contract.CodeNonIntegerNumber)
	add("integral-fraction-money", act, rep(`"amount_minor":60000000`, `"amount_minor":60000000.0`), contract.CodeNonIntegerNumber)
	add("exponent-money", act, rep(`"amount_minor":60000000`, `"amount_minor":6e7`), contract.CodeNonIntegerNumber)
	add("negative-zero", act, rep(`"supplier_revision":1`, `"supplier_revision":-0`), contract.CodeNonIntegerNumber)
	add("overflow-money", act, rep(`"amount_minor":60000000`, `"amount_minor":9007199254740992`), contract.CodeNumberOutOfRange)
	add("overflow-quantity", act, rep(`"quantity":3`, `"quantity":18446744073709551616`), contract.CodeNumberOutOfRange)

	// Version, profile, closed fields, currency.
	add("unsupported-schema-version", act, mut(act, func(m map[string]any) { m["schema_version"] = "2" }), contract.CodeUnsupportedVersion)
	add("numeric-schema-version", act, rep(`"schema_version":"1"`, `"schema_version":1`), contract.CodeUnsupportedVersion)
	add("unsupported-crypto-profile", act, mut(act, func(m map[string]any) { m["crypto_profile"] = "SM-JWS-ES384-1" }), contract.CodeUnsupportedProfile)
	add("premature-obligation-id", act, mut(act, func(m map[string]any) { m["obligation_id"] = "obligation-001" }), contract.CodeUnknownField)
	add("unknown-line-field", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{map[string]any{"item_id": "item-a", "quantity": json.Number("3"), "unit_price_minor": json.Number("20000000"), "discount_minor": json.Number("1")}}
	}), contract.CodeUnknownField)
	add("unsupported-currency", act, mut(act, func(m map[string]any) { m["currency"] = "USD" }), contract.CodeUnsupportedCurrency)

	// Timestamps.
	ts := func(name, field, value string) {
		add(name, man, mut(man, func(m map[string]any) { m[field] = value }), contract.CodeInvalidTimestamp)
	}
	ts("invalid-calendar-date", "expires_at", "2026-02-30T10:00:00Z")
	ts("non-leap-february-29", "expires_at", "2026-02-29T10:00:00Z")
	ts("non-utc-offset", "issued_at", "2026-10-09T15:30:00+05:30")
	ts("missing-zone", "issued_at", "2026-10-09T10:00:00")
	ts("fractional-seconds", "issued_at", "2026-10-09T10:00:00.500Z")
	ts("leap-second", "expires_at", "2026-12-31T23:59:60Z")
	ts("lowercase-z", "issued_at", "2026-10-09T10:00:00z")
	ts("hour-24", "expires_at", "2026-10-09T24:00:00Z")

	// Schema.
	add("negative-money", act, mut(act, func(m map[string]any) { m["amount_minor"] = json.Number("-60000000") }), contract.CodeSchemaViolation)
	add("string-money", act, mut(act, func(m map[string]any) { m["amount_minor"] = "60000000" }), contract.CodeSchemaViolation)
	add("zero-quantity", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{line("item-a", 0, 1)}
		m["amount_minor"] = json.Number("1")
	}), contract.CodeSchemaViolation)
	add("non-ascii-identifier", act, mut(act, func(m map[string]any) { m["tenant_id"] = "synthetic-örg" }), contract.CodeSchemaViolation)
	add("missing-required-field", per, mut(per, func(m map[string]any) { delete(m, "action_digest") }), contract.CodeSchemaViolation)
	add("uppercase-digest", per, mut(per, func(m map[string]any) { m["action_digest"] = strings.Repeat("A", 64) }), contract.CodeSchemaViolation)
	add("duplicate-actor-ids", man, mut(man, func(m map[string]any) { m["actor_ids"] = []any{"agent-a", "agent-a"} }), contract.CodeSchemaViolation)
	add("execution-status-rejected", exe, mut(exe, func(m map[string]any) { m["status"] = "REJECTED" }), contract.CodeSchemaViolation)
	add("top-level-array", act, []byte("["+A+"]"), contract.CodeSchemaViolation)

	// Semantics.
	add("duplicate-item-id", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{line("item-a", 1, 20000000), line("item-a", 2, 20000000)}
	}), contract.CodeDuplicateLineItem)
	add("unsorted-lines", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{line("item-b", 1, 20000000), line("item-a", 2, 20000000)}
	}), contract.CodeLinesNotCanonical)
	add("amount-mismatch", act, mut(act, func(m map[string]any) { m["amount_minor"] = json.Number("60000001") }), contract.CodeAmountMismatch)
	add("line-product-overflow", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{line("item-a", 1<<52, 4)}
		m["amount_minor"] = json.Number("1")
	}), contract.CodeArithmeticOverflow)
	add("line-sum-overflow", act, mut(act, func(m map[string]any) {
		m["lines"] = []any{line("item-a", contract.MaxSafeInteger, 1), line("item-b", 1, 1)}
		m["amount_minor"] = json.Number("1")
	}), contract.CodeArithmeticOverflow)
	add("mandate-expires-at-issue", man, mut(man, func(m map[string]any) { m["expires_at"] = m["issued_at"] }), contract.CodeInvalidTimeRange)
	add("permit-lifetime-over-5-minutes", per, mut(per, func(m map[string]any) { m["expires_at"] = "2026-10-09T10:06:01Z" }), contract.CodeInvalidTimeRange)
	add("observation-match-different-digest", obs, mut(obs, func(m map[string]any) { m["observed_action_digest"] = strings.Repeat("4", 64) }), contract.CodeObservationInconsistent)
	add("observation-match-null-digest", obs, mut(obs, func(m map[string]any) { m["observed_action_digest"] = nil }), contract.CodeObservationInconsistent)
	add("observation-insufficient-with-digest", obs, mut(obs, func(m map[string]any) { m["status"] = "INSUFFICIENT_EVIDENCE" }), contract.CodeObservationInconsistent)
}

func addJWSPositive(vf *vectorFile, name string, k contract.Kind, canonical string, s key) error {
	jws, err := contract.Sign(k, []byte(canonical), contract.Signer{Kid: s.entry.Kid, Key: s.priv})
	if err != nil {
		return err
	}
	header, _ := contract.ProtectedHeader(k, s.entry.Kid)
	input, _ := contract.SigningInput(k, s.entry.Kid, []byte(canonical))
	vf.JWSPositive = append(vf.JWSPositive, jwsPositive{
		Name: name, Kind: k, Kid: s.entry.Kid, ProtectedHeader: string(header), SigningInput: input, JWS: jws,
		PayloadDigest: contract.Digest(k, []byte(canonical)),
	})
	return nil
}

// rawSign builds a compact JWS over arbitrary header and payload bytes. It is
// used only to construct negative vectors that a conforming signer must never
// produce (unpinned headers, extra members, non-canonical payloads).
func rawSign(priv *ecdsa.PrivateKey, header, payload []byte) string {
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	h := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig)
}

func addJWSNegatives(vf *vectorFile, packets map[contract.Kind]map[string]any, signer, observerOnly, attacker key) error {
	per := contract.KindPermit
	P := canonicalJSON(packets[per])
	K := signer.entry.Kid
	hdr := func(s string) []byte { return []byte(s) }
	pinned, _ := contract.ProtectedHeader(per, K)
	good := rawSign(signer.priv, pinned, []byte(P))
	parts := strings.Split(good, ".")
	add := func(name string, k contract.Kind, jws string, code contract.Code) {
		vf.JWSNegative = append(vf.JWSNegative, jwsNegative{Name: name, Kind: k, JWS: jws, ExpectedCode: code})
	}

	// Algorithm attacks.
	add("alg-none", per, b64.EncodeToString(hdr(`{"alg":"none","kid":"`+K+`","typ":"SM-PERMIT+JWS"}`))+"."+parts[1]+".", contract.CodeJWSAlgUnsupported)
	hsHeader := hdr(`{"alg":"HS256","kid":"` + K + `","typ":"SM-PERMIT+JWS"}`)
	pubJSON, _ := json.Marshal(signer.entry.JWK)
	mac := hmac.New(sha256.New, pubJSON)
	hsInput := b64.EncodeToString(hsHeader) + "." + parts[1]
	mac.Write([]byte(hsInput))
	add("alg-hs256-public-key-confusion", per, hsInput+"."+b64.EncodeToString(mac.Sum(nil)), contract.CodeJWSAlgUnsupported)
	es384 := make([]byte, 96)
	_, _ = rand.Read(es384)
	add("alg-es384", per, b64.EncodeToString(hdr(`{"alg":"ES384","kid":"`+K+`","typ":"SM-PERMIT+JWS"}`))+"."+parts[1]+"."+b64.EncodeToString(es384), contract.CodeJWSAlgUnsupported)
	add("alg-missing", per, rawSign(signer.priv, hdr(`{"kid":"`+K+`","typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSAlgUnsupported)

	// Unsupported header members (signed by the trusted key).
	add("crit-header", per, rawSign(signer.priv, hdr(`{"alg":"ES256","crit":["sm-ext"],"kid":"`+K+`","sm-ext":true,"typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSHeaderUnsupported)
	add("jku-remote-key-url", per, rawSign(signer.priv, hdr(`{"alg":"ES256","jku":"https://attacker.invalid/jwks.json","kid":"`+K+`","typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSHeaderUnsupported)
	// CodeQL go/unsafe-quoting (alert #3) is a false positive here: attackerJWK is
	// json.Marshal output, so it is already valid, escaped JSON, and it is placed
	// as a JSON value, not inside a quoted string. The JWK is generated by this
	// offline tool; no external input reaches this header.
	attackerJWK, _ := json.Marshal(attacker.entry.JWK)
	add("embedded-jwk", per, rawSign(attacker.priv, hdr(`{"alg":"ES256","jwk":`+string(attackerJWK)+`,"typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSHeaderUnsupported)
	add("x5u-header", per, rawSign(signer.priv, hdr(`{"alg":"ES256","kid":"`+K+`","typ":"SM-PERMIT+JWS","x5u":"https://attacker.invalid/cert.pem"}`), []byte(P)), contract.CodeJWSHeaderUnsupported)

	// Type confusion.
	obsJWS, err := contract.Sign(contract.KindObservation, []byte(canonicalJSON(packets[contract.KindObservation])), contract.Signer{Kid: K, Key: signer.priv})
	if err != nil {
		return err
	}
	add("observation-presented-as-permit", per, obsJWS, contract.CodeJWSTypeMismatch)
	add("typ-missing", per, rawSign(signer.priv, hdr(`{"alg":"ES256","kid":"`+K+`"}`), []byte(P)), contract.CodeJWSTypeMismatch)
	add("typ-generic-jwt", per, rawSign(signer.priv, hdr(`{"alg":"ES256","kid":"`+K+`","typ":"JWT"}`), []byte(P)), contract.CodeJWSTypeMismatch)

	// Key trust.
	attackerHeader, _ := contract.ProtectedHeader(per, attacker.entry.Kid)
	add("untrusted-key", per, rawSign(attacker.priv, attackerHeader, []byte(P)), contract.CodeJWSKeyUntrusted)
	obsHeader, _ := contract.ProtectedHeader(per, observerOnly.entry.Kid)
	add("key-not-allowed-for-typ", per, rawSign(observerOnly.priv, obsHeader, []byte(P)), contract.CodeJWSKeyUntrusted)
	add("kid-missing", per, rawSign(signer.priv, hdr(`{"alg":"ES256","typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSKeyUntrusted)
	add("trusted-kid-signed-by-attacker", per, rawSign(attacker.priv, pinned, []byte(P)), contract.CodeJWSSignatureInvalid)

	// Header form.
	add("header-with-whitespace", per, rawSign(signer.priv, hdr(`{"alg": "ES256", "kid": "`+K+`", "typ": "SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSMalformed)
	add("header-member-order", per, rawSign(signer.priv, hdr(`{"typ":"SM-PERMIT+JWS","kid":"`+K+`","alg":"ES256"}`), []byte(P)), contract.CodeJWSMalformed)
	add("header-duplicate-member", per, rawSign(signer.priv, hdr(`{"alg":"ES256","alg":"ES256","kid":"`+K+`","typ":"SM-PERMIT+JWS"}`), []byte(P)), contract.CodeJWSMalformed)

	// Tampering after signing.
	tampered := strings.Replace(P, `"amount_minor":60000000`, `"amount_minor":60000001`, 1)
	add("tampered-amount", per, parts[0]+"."+b64.EncodeToString([]byte(tampered))+"."+parts[2], contract.CodeJWSSignatureInvalid)
	forged := strings.Replace(P, strings.Repeat("0", 64), strings.Repeat("5", 64), 1)
	add("forged-action-digest", per, parts[0]+"."+b64.EncodeToString([]byte(forged))+"."+parts[2], contract.CodeJWSSignatureInvalid)
	sig, _ := b64.DecodeString(parts[2])
	flipped := append([]byte(nil), sig...)
	flipped[10] ^= 0x01
	add("bit-flipped-signature", per, parts[0]+"."+parts[1]+"."+b64.EncodeToString(flipped), contract.CodeJWSSignatureInvalid)
	add("truncated-signature", per, parts[0]+"."+parts[1]+"."+b64.EncodeToString(sig[:63]), contract.CodeJWSSignatureInvalid)
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	der, _ := ecdsa.SignASN1(rand.Reader, signer.priv, h[:])
	add("der-encoded-signature", per, parts[0]+"."+parts[1]+"."+b64.EncodeToString(der), contract.CodeJWSSignatureInvalid)
	add("zero-signature", per, parts[0]+"."+parts[1]+"."+b64.EncodeToString(make([]byte, 64)), contract.CodeJWSSignatureInvalid)

	// Encoding and structure.
	add("padded-base64", per, good+"=", contract.CodeJWSMalformed)
	add("standard-base64-character", per, parts[0]+"."+parts[1]+"."+parts[2][:len(parts[2])-1]+"+", contract.CodeJWSMalformed)
	add("two-segments", per, parts[0]+"."+parts[1], contract.CodeJWSMalformed)
	add("four-segments", per, good+"."+parts[2], contract.CodeJWSMalformed)
	add("empty-string", per, "", contract.CodeJWSMalformed)
	add("json-serialization", per, `{"payload":"`+parts[1]+`","protected":"`+parts[0]+`","signature":"`+parts[2]+`"}`, contract.CodeJWSMalformed)

	// Correctly signed by a trusted key, but the payload breaks the contract.
	add("noncanonical-payload-whitespace", per, rawSign(signer.priv, pinned, []byte(messy(packets[per], " "))), contract.CodeJWSPayloadNotCanonical)
	add("noncanonical-payload-key-order", per, rawSign(signer.priv, pinned, []byte(messy(packets[per], ""))), contract.CodeJWSPayloadNotCanonical)
	add("signed-duplicate-key", per, rawSign(signer.priv, pinned, []byte(`{"amount_minor":1,`+P[1:])), contract.CodeDuplicateKey)
	add("signed-fractional-amount", per, rawSign(signer.priv, pinned, []byte(strings.Replace(P, `"amount_minor":60000000`, `"amount_minor":60000000.0`, 1))), contract.CodeNonIntegerNumber)
	missing := clone(packets[per])
	delete(missing, "control_epoch")
	add("signed-missing-field", per, rawSign(signer.priv, pinned, []byte(canonicalJSON(missing))), contract.CodeSchemaViolation)
	long := clone(packets[per])
	long["expires_at"] = "2026-10-09T11:01:00Z"
	add("signed-permit-lifetime-over-5-minutes", per, rawSign(signer.priv, pinned, []byte(canonicalJSON(long))), contract.CodeInvalidTimeRange)
	return nil
}

func check(vf *vectorFile) error {
	tsBytes, _ := json.Marshal(vf.TrustStore)
	trust, err := keys.ParseTrustStore(tsBytes)
	if err != nil {
		return err
	}
	for _, v := range vf.Canonical {
		if _, verr := contract.Validate(v.Kind, []byte(v.Input)); verr != nil {
			return fmt.Errorf("canonical %s: %v", v.Name, verr)
		}
		if v.Input == v.Canonical {
			return fmt.Errorf("canonical %s: input is already canonical", v.Name)
		}
	}
	for _, v := range vf.PayloadNegative {
		in, _ := b64.DecodeString(v.InputB64)
		_, verr := contract.Validate(v.Kind, in)
		if verr == nil || verr.Code != v.ExpectedCode {
			return fmt.Errorf("payload negative %s: got %v, want %s", v.Name, verr, v.ExpectedCode)
		}
	}
	for _, v := range vf.JWSPositive {
		if _, verr := contract.Verify(v.JWS, v.Kind, trust); verr != nil {
			return fmt.Errorf("jws positive %s: %v", v.Name, verr)
		}
	}
	for _, v := range vf.JWSNegative {
		_, verr := contract.Verify(v.JWS, v.Kind, trust)
		if verr == nil || verr.Code != v.ExpectedCode {
			return fmt.Errorf("jws negative %s: got %v, want %s", v.Name, verr, v.ExpectedCode)
		}
	}
	return nil
}

// decode parses packet JSON keeping integers exact.
func decode(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	c, _ := decode(b)
	return c
}

func line(id string, q, p int64) map[string]any {
	return map[string]any{"item_id": id, "quantity": json.Number(fmt.Sprint(q)), "unit_price_minor": json.Number(fmt.Sprint(p))}
}

// canonicalJSON returns RFC 8785 bytes of v (v contains only strings, exact
// integers, nulls, arrays and objects).
func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c, err := contract.Canonicalize(b)
	if err != nil {
		panic(err)
	}
	return string(c)
}

// messy serialises v validly but non-canonically: reverse key order, the
// separator sep around tokens, and every lowercase "a" in strings escaped as
// a, so canonicalisation must reorder keys, drop whitespace and unescape.
func messy(v any, sep string) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(keys)))
			b.WriteString("{" + sep)
			for i, k := range keys {
				if i > 0 {
					b.WriteString(sep + "," + sep)
				}
				writeMessyString(&b, k)
				b.WriteString(sep + ":" + sep)
				walk(t[k])
			}
			b.WriteString(sep + "}")
		case []any:
			b.WriteString("[" + sep)
			for i, e := range t {
				if i > 0 {
					b.WriteString(sep + "," + sep)
				}
				walk(e)
			}
			b.WriteString(sep + "]")
		case string:
			writeMessyString(&b, t)
		case nil:
			b.WriteString("null")
		default:
			x, _ := json.Marshal(t)
			b.Write(x)
		}
	}
	walk(v)
	if sep == "" {
		return strings.ReplaceAll(b.String(), ",", ", ")
	}
	return b.String()
}

func writeMessyString(b *strings.Builder, s string) {
	x, _ := json.Marshal(s)
	b.WriteString(strings.ReplaceAll(string(x), "a", `\u0061`))
}

func isPrintableUTF8(b []byte) bool {
	s := string(b)
	return strings.ToValidUTF8(s, "�") == s && !strings.HasPrefix(s, "\uFEFF")
}

// ---- evidence bundle fixture (I07) ----

type evidenceCase struct {
	Name   string         `json:"name"`
	Bundle map[string]any `json:"bundle"`
	Expect map[string]any `json:"expect"`
}

type evidenceFile struct {
	FixtureSet string           `json:"fixture_set"`
	Note       string           `json:"note"`
	TrustStore *keys.TrustStore `json:"trust_store"`
	UsableAt   string           `json:"usable_at"`
	Cases      []evidenceCase   `json:"cases"`
}

// runEvidence builds a complete, consistently linked synthetic evidence chain
// from the packet examples and three variants, signed by a fresh key.
func runEvidence(out, packetDir string) error {
	signer, err := newKey("evidence-fixture-signer", []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"})
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
	mandate, err := load(contract.KindMandate)
	if err != nil {
		return err
	}
	action, _ := load(contract.KindAction)
	permit, _ := load(contract.KindPermit)
	execution, _ := load(contract.KindExecution)
	observation, _ := load(contract.KindObservation)

	dig := func(k contract.Kind, m map[string]any) string { return contract.Digest(k, []byte(canonicalJSON(m))) }
	sign := func(k contract.Kind, m map[string]any) (string, error) {
		return contract.Sign(k, []byte(canonicalJSON(m)), contract.Signer{Kid: signer.entry.Kid, Key: signer.priv})
	}
	actionDigest := dig(contract.KindAction, action)
	permit["action_digest"] = actionDigest
	permit["mandate_digest"] = dig(contract.KindMandate, mandate)
	execution["action_digest"] = actionDigest
	execution["permit_digest"] = dig(contract.KindPermit, permit)
	observation["execution_digest"] = dig(contract.KindExecution, execution)
	observation["expected_action_digest"] = actionDigest
	observation["observed_action_digest"] = actionDigest

	build := func(exec, obs map[string]any) (map[string]any, error) {
		recs := map[string]any{}
		for _, r := range []struct {
			k contract.Kind
			m map[string]any
		}{{contract.KindMandate, mandate}, {contract.KindPermit, permit}, {contract.KindExecution, exec}, {contract.KindObservation, obs}} {
			jws, err := sign(r.k, r.m)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.k, err)
			}
			recs[string(r.k)+"_jws"] = jws
		}
		return map[string]any{
			"operation_id": permit["operation_id"], "evidence": "AVAILABLE", "records": recs, "action": action,
			"links": map[string]any{"instruction_id": action["instruction_id"], "action_digest": actionDigest,
				"mandate_digest": permit["mandate_digest"], "permit_digest": dig(contract.KindPermit, permit),
				"execution_digest": dig(contract.KindExecution, exec), "obligation_id": exec["obligation_id"]},
			"incomplete": []any{},
		}, nil
	}

	complete, err := build(execution, observation)
	if err != nil {
		return err
	}
	// Broken: execution validly signed but bound to a different permit digest.
	brokenExec := clone(execution)
	brokenExec["permit_digest"] = strings.Repeat("f", 64)
	brokenObs := clone(observation)
	brokenObs["execution_digest"] = dig(contract.KindExecution, brokenExec)
	broken, err := build(brokenExec, brokenObs)
	if err != nil {
		return err
	}
	// Mismatch: the observer saw a different action at the destination.
	mismatchObs := clone(observation)
	mismatchObs["status"] = "MISMATCH"
	mismatchObs["observed_action_digest"] = strings.Repeat("e", 64)
	mismatch, err := build(execution, mismatchObs)
	if err != nil {
		return err
	}
	// Incomplete: execution and observation not yet available.
	incomplete := clone(complete)
	recs := incomplete["records"].(map[string]any)
	delete(recs, "execution_jws")
	delete(recs, "observation_jws")
	incomplete["evidence"] = "PENDING"
	incomplete["incomplete"] = []any{"execution_jws", "observation_jws"}

	ef := evidenceFile{
		FixtureSet: "steelmatter-evidence-v1",
		Note:       "Synthetic evidence chains built from the packet examples (docs/evidence.md). The private key was discarded. usable_at lies inside the permit window; at any later time the permit verifies as historical evidence but is EXPIRED for use.",
		TrustStore: &keys.TrustStore{Version: keys.TrustStoreVer, Keys: []keys.TrustedKey{signer.entry}},
		UsableAt:   "2026-10-09T10:03:00Z",
		Cases: []evidenceCase{
			{"complete", complete, map[string]any{"linkage": "COMPLETE", "observation": "MATCH", "observed_matches_action": true}},
			{"broken-permit-link", broken, map[string]any{"linkage": "BROKEN", "broken_check": "permit_digest:execution"}},
			{"observer-mismatch", mismatch, map[string]any{"linkage": "COMPLETE", "observation": "MISMATCH", "observed_matches_action": false}},
			{"incomplete-pending", incomplete, map[string]any{"linkage": "INCOMPLETE", "observation": "NOT_OBSERVED"}},
		},
	}
	b, err := json.MarshalIndent(ef, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d evidence cases\n", out, len(ef.Cases))
	return nil
}

// ---- trust-store parsing fixture (QA-D1-002) ----

type trustCase struct {
	Name   string `json:"name"`
	Expect string `json:"expect"` // "accept" or "reject"
	Store  string `json:"store"`  // raw JSON, so duplicate members can be expressed
}

// runTrustStore writes accept/reject cases that the Go and TypeScript trust
// store parsers must agree on. Each case is checked against Go first.
func runTrustStore(out string) error {
	a, err := newKey("key-a", []string{"SM-PERMIT+JWS"})
	if err != nil {
		return err
	}
	b, err := newKey("key-b", []string{"SM-OBSERVATION+JWS"})
	if err != nil {
		return err
	}
	entry := func(k keys.TrustedKey) string {
		j, _ := json.Marshal(k)
		return string(j)
	}
	store := func(entries ...string) string {
		return `{"trust_store_version":"1","keys":[` + strings.Join(entries, ",") + `]}`
	}
	renamed := a.entry
	renamed.Name = "key-a-again"
	sameName := b.entry
	sameName.Name = a.entry.Name
	widerPurpose := a.entry
	widerPurpose.Name, widerPurpose.Purposes = "key-a-wide", []string{"SM-PERMIT+JWS", "SM-MANDATE+JWS"}
	noPurpose := a.entry
	noPurpose.Purposes = []string{}
	wrongKid := a.entry
	wrongKid.Kid = b.entry.Kid
	es384 := a.entry
	es384.Alg = "ES384"
	offCurve := a.entry
	offCurve.JWK.Y = offCurve.JWK.X
	offCurve.Kid = keys.Thumbprint(offCurve.JWK)
	ea := entry(a.entry)
	cases := []trustCase{
		{"valid-two-keys", "accept", store(ea, entry(b.entry))},
		{"valid-empty", "accept", store()},
		{"duplicate-kid", "reject", store(ea, entry(renamed))},
		{"duplicate-kid-different-purposes", "reject", store(ea, entry(widerPurpose))},
		{"duplicate-name", "reject", store(ea, entry(sameName))},
		{"duplicate-member-in-entry", "reject", store(strings.Replace(ea, `"kid":`, `"kid":"x","kid":`, 1))},
		{"duplicate-top-level-member", "reject", `{"trust_store_version":"1","trust_store_version":"1","keys":[]}`},
		{"case-variant-field", "reject", store(strings.Replace(ea, `"kid":`, `"KID":`, 1))},
		{"unknown-store-field", "reject", `{"trust_store_version":"1","keys":[],"jku":"https://attacker.invalid/keys"}`},
		{"unknown-key-field", "reject", store(strings.Replace(ea, `"kid":`, `"x5u":"https://attacker.invalid","kid":`, 1))},
		{"missing-field", "reject", store(strings.Replace(ea, `"synthetic":true,`, ``, 1))},
		{"empty-purposes", "reject", store(entry(noPurpose))},
		{"kid-not-thumbprint", "reject", store(entry(wrongKid))},
		{"wrong-alg", "reject", store(entry(es384))},
		{"point-off-curve", "reject", store(entry(offCurve))},
		{"invalid-created-at", "reject", store(strings.Replace(ea, a.entry.CreatedAt.Format(`"2006-01-02T15:04:05Z07:00"`), `"2026-02-30T00:00:00Z"`, 1))},
		{"unsupported-version", "reject", `{"trust_store_version":"2","keys":[]}`},
		// QA-D1-003: null values and out-of-range zone offsets.
		{"null-name", "reject", store(withField(ea, "name", nil))},
		{"null-kid", "reject", store(withField(ea, "kid", nil))},
		{"null-synthetic", "reject", store(withField(ea, "synthetic", nil))},
		{"null-created-at", "reject", store(withField(ea, "created_at", nil))},
		{"null-purpose-item", "reject", store(withField(ea, "purposes", []any{"SM-PERMIT+JWS", nil}))},
		{"null-jwk-coordinate", "reject", store(withJWK(ea, "x", nil))},
		{"string-synthetic", "reject", store(withField(ea, "synthetic", "true"))},
		{"created-at-offset-hour-24", "reject", store(withField(ea, "created_at", "2026-10-12T09:00:00+24:00"))},
		{"created-at-offset-minute-60", "reject", store(withField(ea, "created_at", "2026-10-12T09:00:00+23:60"))},
		{"created-at-offset-ok", "accept", store(withField(ea, "created_at", "2026-10-12T09:00:00+05:30"))},
		{"created-at-fraction-ok", "accept", store(withField(ea, "created_at", "2026-10-12T09:00:00.123456789Z"))},
		// CLR-03: purposes must be distinct exact JWS types.
		{"purpose-unknown", "reject", store(withField(ea, "purposes", []any{"SM-ACTION+JWS"}))},
		{"purpose-unknown-alongside-valid", "reject", store(withField(ea, "purposes", []any{"SM-PERMIT+JWS", "SM-ACTION+JWS"}))},
		{"purpose-empty-string", "reject", store(withField(ea, "purposes", []any{""}))},
		{"purpose-lowercase", "reject", store(withField(ea, "purposes", []any{"sm-permit+jws"}))},
		{"purpose-mixed-case", "reject", store(withField(ea, "purposes", []any{"SM-Permit+JWS"}))},
		{"purpose-leading-space", "reject", store(withField(ea, "purposes", []any{" SM-PERMIT+JWS"}))},
		{"purpose-trailing-space", "reject", store(withField(ea, "purposes", []any{"SM-PERMIT+JWS "}))},
		{"purpose-near-miss", "reject", store(withField(ea, "purposes", []any{"SM-PERMIT-JWS"}))},
		{"purpose-repeated", "reject", store(withField(ea, "purposes", []any{"SM-PERMIT+JWS", "SM-PERMIT+JWS"}))},
		{"purpose-non-string", "reject", store(withField(ea, "purposes", []any{1}))},
		// Historical trust keys: optional not_after (retired-from).
		{"not-after-ok", "accept", store(withField(ea, "not_after", a.entry.CreatedAt.Add(24*time.Hour).Format(time.RFC3339)))},
		{"not-after-offset-ok", "accept", store(withField(ea, "not_after", a.entry.CreatedAt.Add(24*time.Hour).In(time.FixedZone("IST", 19800)).Format(time.RFC3339)))},
		{"not-after-null", "reject", store(withField(ea, "not_after", nil))},
		{"not-after-invalid-date", "reject", store(withField(ea, "not_after", "2026-02-30T00:00:00Z"))},
		{"not-after-before-created-at", "reject", store(withField(ea, "not_after", a.entry.CreatedAt.Add(-24*time.Hour).Format(time.RFC3339)))},
		{"not-after-equal-created-at", "reject", store(withField(ea, "not_after", a.entry.CreatedAt.Format(time.RFC3339)))},
		{"purpose-all-four-ok", "accept", store(withField(ea, "purposes", []any{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"}))},
		{"trailing-content", "reject", store() + ` {}`},
	}
	for _, c := range cases {
		_, err := keys.ParseTrustStore([]byte(c.Store))
		if got := map[bool]string{true: "accept", false: "reject"}[err == nil]; got != c.Expect {
			return fmt.Errorf("trust-store case %s: Go %s (%v); want %s", c.Name, got, err, c.Expect)
		}
		if c.Name == "invalid-created-at" && !strings.Contains(c.Store, "2026-02-30") {
			return fmt.Errorf("invalid-created-at case was not constructed")
		}
	}
	doc := map[string]any{
		"fixture_set": "steelmatter-truststore-v1",
		"note":        "Accept/reject cases for trust-store parsing that Go keys.ParseTrustStore and TypeScript loadTrustStore must agree on (QA-D1-002). Private keys discarded.",
		"cases":       cases,
	}
	b2, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b2, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d trust-store cases\n", out, len(cases))
	return nil
}

// withField returns the JSON entry with field set to v (nil = JSON null).
func withField(entry, field string, v any) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(entry), &m)
	m[field] = v
	b, _ := json.Marshal(m)
	return string(b)
}

// withJWK returns the JSON entry with jwk member set to v.
func withJWK(entry, member string, v any) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(entry), &m)
	m["jwk"].(map[string]any)[member] = v
	b, _ := json.Marshal(m)
	return string(b)
}
