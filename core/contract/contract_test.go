package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
)

func requireCode(t *testing.T, err *Error, want Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want %s", want)
	}
	if err.Code != want {
		t.Fatalf("code %s (%v); want %s", err.Code, err, want)
	}
}

func TestParseStrictRejections(t *testing.T) {
	cases := []struct {
		name, in string
		want     Code
	}{
		{"duplicate key", `{"a":1,"a":1}`, CodeDuplicateKey},
		{"duplicate nested key", `{"a":{"b":1,"b":2}}`, CodeDuplicateKey},
		{"duplicate key via escape", `{"a":1,"\u0061":1}`, CodeDuplicateKey},
		{"fraction", `{"a":1.5}`, CodeNonIntegerNumber},
		{"integral fraction", `{"a":1.0}`, CodeNonIntegerNumber},
		{"exponent", `{"a":1e3}`, CodeNonIntegerNumber},
		{"negative zero", `{"a":-0}`, CodeNonIntegerNumber},
		{"2^53", `{"a":9007199254740992}`, CodeNumberOutOfRange},
		{"-2^53", `{"a":-9007199254740992}`, CodeNumberOutOfRange},
		{"huge", `{"a":123456789012345678901234567890}`, CodeNumberOutOfRange},
		{"leading zero", `{"a":01}`, CodeInvalidJSON},
		{"trailing comma", `{"a":1,}`, CodeInvalidJSON},
		{"trailing content", `{"a":1} {}`, CodeInvalidJSON},
		{"single quotes", `{'a':1}`, CodeInvalidJSON},
		{"control char", "{\"a\":\"x\ty\"}", CodeInvalidJSON},
		{"NaN", `{"a":NaN}`, CodeInvalidJSON},
		{"empty", ``, CodeInvalidJSON},
		{"lone high surrogate", `{"a":"\ud800"}`, CodeInvalidEncoding},
		{"lone low surrogate", `{"a":"\udc00"}`, CodeInvalidEncoding},
		{"reversed surrogates", `{"a":"\udc00\ud800"}`, CodeInvalidEncoding},
		{"invalid utf8", "{\"a\":\"\xff\"}", CodeInvalidEncoding},
		{"bom", "\xef\xbb\xbf{}", CodeInvalidEncoding},
		{"too deep", strings.Repeat("[", MaxDepth+2) + strings.Repeat("]", MaxDepth+2), CodeInvalidJSON},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseStrict([]byte(c.in))
			requireCode(t, err, c.want)
		})
	}
}

func TestParseStrictAccepts(t *testing.T) {
	v, err := ParseStrict([]byte(` {"s":"\u00e9\ud83d\ude00\/","n":-9007199254740991,"m":9007199254740991,"z":0,"b":[true,false,null],"e":{}} `))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["s"] != "é😀/" || m["n"] != int64(-MaxSafeInteger) || m["m"] != MaxSafeInteger || m["z"] != int64(0) {
		t.Fatalf("parsed %#v", m)
	}
}

func TestParseStrictSizeLimit(t *testing.T) {
	big := append([]byte(`{"a":"`), bytes.Repeat([]byte("x"), MaxPayloadBytes)...)
	big = append(big, `"}`...)
	_, err := ParseStrict(big)
	requireCode(t, err, CodePayloadTooLarge)
}

func TestParseTimestamp(t *testing.T) {
	for _, ok := range []string{"2026-10-09T10:00:00Z", "2028-02-29T23:59:59Z", "2026-12-31T00:00:00Z"} {
		if _, err := ParseTimestamp(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"2026-02-29T10:00:00Z", "2026-02-30T10:00:00Z", "2026-13-01T00:00:00Z", "2026-10-09T24:00:00Z",
		"2026-12-31T23:59:60Z", "2026-10-09T10:00:00.5Z", "2026-10-09T10:00:00+05:30", "2026-10-09T10:00:00z",
		"2026-10-09 10:00:00Z", "2026-10-09T10:00:00", "+2026-10-09T10:00:00Z",
	} {
		if _, err := ParseTimestamp(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func packet(t *testing.T, k Kind) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../fixtures/packet/" + string(k) + ".payload.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mutate(t *testing.T, k Kind, f func(m map[string]any)) []byte {
	t.Helper()
	m := packet(t, k)
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPacketPositiveFixturesValidate(t *testing.T) {
	for _, k := range Kinds {
		if _, err := Validate(k, mutate(t, k, func(map[string]any) {})); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestValidateRejections(t *testing.T) {
	line := func(id string, q, p int64) map[string]any {
		return map[string]any{"item_id": id, "quantity": q, "unit_price_minor": p}
	}
	cases := []struct {
		name string
		kind Kind
		f    func(m map[string]any)
		want Code
	}{
		{"unsupported version", KindAction, func(m map[string]any) { m["schema_version"] = "2" }, CodeUnsupportedVersion},
		{"missing version", KindAction, func(m map[string]any) { delete(m, "schema_version") }, CodeUnsupportedVersion},
		{"unsupported profile", KindPermit, func(m map[string]any) { m["crypto_profile"] = "SM-JWS-ES384-1" }, CodeUnsupportedProfile},
		{"premature obligation_id", KindAction, func(m map[string]any) { m["obligation_id"] = "x" }, CodeUnknownField},
		{"unknown line field", KindAction, func(m map[string]any) {
			m["lines"] = []any{map[string]any{"item_id": "item-a", "quantity": 3, "unit_price_minor": 20000000, "discount": 1}}
		}, CodeUnknownField},
		{"USD", KindAction, func(m map[string]any) { m["currency"] = "USD" }, CodeUnsupportedCurrency},
		{"non-Z timestamp", KindMandate, func(m map[string]any) { m["issued_at"] = "2026-10-09T15:30:00+05:30" }, CodeInvalidTimestamp},
		{"invalid calendar date", KindMandate, func(m map[string]any) { m["expires_at"] = "2026-02-30T10:00:00Z" }, CodeInvalidTimestamp},
		{"negative amount", KindAction, func(m map[string]any) { m["amount_minor"] = -1 }, CodeSchemaViolation},
		{"zero quantity", KindAction, func(m map[string]any) { m["lines"] = []any{line("item-a", 0, 1)}; m["amount_minor"] = 1 }, CodeSchemaViolation},
		{"missing field", KindPermit, func(m map[string]any) { delete(m, "action_digest") }, CodeSchemaViolation},
		{"uppercase digest", KindPermit, func(m map[string]any) { m["action_digest"] = strings.Repeat("A", 64) }, CodeSchemaViolation},
		{"duplicate actor", KindMandate, func(m map[string]any) { m["actor_ids"] = []any{"agent-a", "agent-a"} }, CodeSchemaViolation},
		{"duplicate item", KindAction, func(m map[string]any) {
			m["lines"] = []any{line("item-a", 1, 1), line("item-a", 2, 1)}
			m["amount_minor"] = 3
		}, CodeDuplicateLineItem},
		{"unsorted lines", KindAction, func(m map[string]any) {
			m["lines"] = []any{line("item-b", 1, 1), line("item-a", 1, 1)}
			m["amount_minor"] = 2
		}, CodeLinesNotCanonical},
		{"duplicate wins over unsorted", KindAction, func(m map[string]any) {
			m["lines"] = []any{line("item-b", 1, 1), line("item-a", 1, 1), line("item-b", 1, 1)}
			m["amount_minor"] = 3
		}, CodeDuplicateLineItem},
		{"amount mismatch", KindAction, func(m map[string]any) { m["amount_minor"] = 60000001 }, CodeAmountMismatch},
		{"product overflow", KindAction, func(m map[string]any) {
			m["lines"] = []any{line("item-a", 1<<52, 4)}
			m["amount_minor"] = 1
		}, CodeArithmeticOverflow},
		{"sum overflow", KindAction, func(m map[string]any) {
			m["lines"] = []any{line("item-a", MaxSafeInteger, 1), line("item-b", 1, 1)}
			m["amount_minor"] = 1
		}, CodeArithmeticOverflow},
		{"mandate expires before issue", KindMandate, func(m map[string]any) { m["expires_at"] = m["issued_at"] }, CodeInvalidTimeRange},
		{"permit longer than 5 minutes", KindPermit, func(m map[string]any) { m["expires_at"] = "2026-10-09T10:06:01Z" }, CodeInvalidTimeRange},
		{"MATCH with different digest", KindObservation, func(m map[string]any) { m["observed_action_digest"] = strings.Repeat("4", 64) }, CodeObservationInconsistent},
		{"MATCH with null digest", KindObservation, func(m map[string]any) { m["observed_action_digest"] = nil }, CodeObservationInconsistent},
		{"INSUFFICIENT with digest", KindObservation, func(m map[string]any) { m["status"] = "INSUFFICIENT_EVIDENCE" }, CodeObservationInconsistent},
		{"MISMATCH with equal digest", KindObservation, func(m map[string]any) { m["status"] = "MISMATCH" }, CodeObservationInconsistent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(c.kind, mutate(t, c.kind, c.f))
			requireCode(t, err, c.want)
		})
	}
}

func TestValidateAcceptsByteOrderedLines(t *testing.T) {
	// Byte order puts uppercase before lowercase; locale-aware sorting would not.
	b := mutate(t, KindAction, func(m map[string]any) {
		m["lines"] = []any{
			map[string]any{"item_id": "Item-Z", "quantity": 1, "unit_price_minor": 2},
			map[string]any{"item_id": "item-a", "quantity": 3, "unit_price_minor": 4},
		}
		m["amount_minor"] = 14
	})
	if _, err := Validate(KindAction, b); err != nil {
		t.Fatal(err)
	}
}

func TestNormaliseLines(t *testing.T) {
	in := []Line{{"item-c", 1, 1}, {"item-a", 2, 2}, {"Item-b", 3, 3}}
	out, err := NormaliseLines(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ItemID != "Item-b" || out[1].ItemID != "item-a" || out[2].ItemID != "item-c" || in[0].ItemID != "item-c" {
		t.Fatalf("normalised %v (input %v)", out, in)
	}
	_, err = NormaliseLines([]Line{{"x", 1, 1}, {"x", 2, 1}})
	requireCode(t, err, CodeDuplicateLineItem)
}

func TestProposalOrderDoesNotChangeDigest(t *testing.T) {
	// T40 contract portion: different proposal orders normalise to one digest.
	digestOf := func(lines []Line) string {
		norm, err := NormaliseLines(lines)
		if err != nil {
			t.Fatal(err)
		}
		b := mutate(t, KindAction, func(m map[string]any) { m["lines"] = norm; m["amount_minor"] = 7 })
		_, d, cerr := CanonicalDigest(KindAction, b)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return d
	}
	a := digestOf([]Line{{"item-a", 1, 3}, {"item-b", 2, 2}})
	b := digestOf([]Line{{"item-b", 2, 2}, {"item-a", 1, 3}})
	if a != b {
		t.Fatalf("digests differ: %s %s", a, b)
	}
}

func TestDigestIsDomainSeparated(t *testing.T) {
	c := []byte(`{"a":1}`)
	if Digest(KindAction, c) == Digest(KindPermit, c) {
		t.Fatal("same digest for different kinds")
	}
	if len(Digest(KindAction, c)) != 64 || strings.ToLower(Digest(KindAction, c)) != Digest(KindAction, c) {
		t.Fatal("digest must be 64 lowercase hex characters")
	}
}

func TestCanonicalizeReordersAndUnescapes(t *testing.T) {
	got, err := Canonicalize([]byte(`{ "b" : [1, "a\/"], "a":{"y":null,"x":true} }`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":{"x":true,"y":null},"b":[1,"a/"]}` {
		t.Fatalf("canonical %s", got)
	}
}

func testTrust(t *testing.T, purposes ...string) (*keys.TrustStore, Signer) {
	t.Helper()
	dir := t.TempDir()
	k, err := keys.Provision(dir, "signer", purposes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	priv, err := keys.ReadPrivateKey(dir + "/signer.key.pem")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := keys.LoadTrustStore(dir + "/trust-store.json")
	if err != nil {
		t.Fatal(err)
	}
	return ts, Signer{Kid: k.Kid, Key: priv}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	ts, s := testTrust(t, "SM-PERMIT+JWS")
	raw := mutate(t, KindPermit, func(map[string]any) {})
	jws, err := Sign(KindPermit, raw, s)
	if err != nil {
		t.Fatal(err)
	}
	v, verr := Verify(jws, KindPermit, ts)
	if verr != nil {
		t.Fatal(verr)
	}
	canonical, _ := Canonicalize(raw)
	if !bytes.Equal(v.Payload, canonical) || v.Digest != Digest(KindPermit, canonical) {
		t.Fatal("verified payload differs from canonical input")
	}
	if v.Value.(*Permit).AmountMinor != 60000000 {
		t.Fatal("typed payload not returned")
	}
}

func TestVerifyRejectsWrongKindAndPurpose(t *testing.T) {
	ts, s := testTrust(t, "SM-PERMIT+JWS")
	jws, err := Sign(KindPermit, mutate(t, KindPermit, func(map[string]any) {}), s)
	if err != nil {
		t.Fatal(err)
	}
	_, verr := Verify(jws, KindExecution, ts)
	requireCode(t, verr, CodeJWSTypeMismatch)

	// The same key may not sign an execution record.
	if _, err := Sign(KindExecution, mutate(t, KindExecution, func(map[string]any) {}), s); err != nil {
		t.Fatal(err)
	}
	ej, _ := Sign(KindExecution, mutate(t, KindExecution, func(map[string]any) {}), s)
	_, verr = Verify(ej, KindExecution, ts)
	requireCode(t, verr, CodeJWSKeyUntrusted)
}

func TestSignRejectsInvalidPayloadAndUnsignedKind(t *testing.T) {
	_, s := testTrust(t, "SM-PERMIT+JWS")
	if _, err := Sign(KindPermit, []byte(`{"a":1,"a":2}`), s); err == nil {
		t.Fatal("invalid payload signed")
	}
	if _, err := Sign(KindAction, mutate(t, KindAction, func(map[string]any) {}), s); err == nil {
		t.Fatal("action signed directly")
	}
}

func TestKeyringRejectsMixedPurposeObservationKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := keys.Provision(dir, "mixed", []string{"SM-OBSERVATION+JWS", "SM-EXECUTION+JWS"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadKeyring(dir); err == nil {
		t.Fatal("mixed-purpose observation key accepted")
	}
}

func TestSplitObserverKeepsObservationKeyOutOfEngine(t *testing.T) {
	all := map[string]Signer{"SM-PERMIT+JWS": {Kid: "p"}, "SM-OBSERVATION+JWS": {Kid: "o"}}
	eng, obs, ok := SplitObserver(all)
	if !ok || obs.Kid != "o" || len(eng) != 1 || eng["SM-PERMIT+JWS"].Kid != "p" {
		t.Fatalf("split %v %v %v", eng, obs, ok)
	}
}

// keys.KnownPurposes (trust-store validation) must equal the signed JWS types.
func TestKnownPurposesMatchSignedTypes(t *testing.T) {
	var types []string
	for _, k := range Kinds {
		if typ, ok := TypeOf(k); ok {
			types = append(types, typ)
		}
	}
	sort.Strings(types)
	known := append([]string(nil), keys.KnownPurposes...)
	sort.Strings(known)
	if strings.Join(types, ",") != strings.Join(known, ",") {
		t.Fatalf("keys.KnownPurposes %v != signed types %v", known, types)
	}
}

func TestKeyringRefusesRetiredSigningKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := keys.Provision(dir, "svc", []string{"SM-PERMIT+JWS"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := dir + "/trust-store.json"
	ts, _ := keys.LoadTrustStore(path)
	if err := ts.Retire("svc", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = ts.Save(path)
	if _, _, err := LoadKeyring(dir); err == nil {
		t.Fatal("retired signing key loaded for signing")
	}
}
