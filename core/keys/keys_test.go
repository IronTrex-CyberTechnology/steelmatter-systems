package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testPurposes = []string{"SM-MANDATE+JWS"}

var fixedNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func TestThumbprintFollowsRFC7638MemberOrder(t *testing.T) {
	j := PublicJWK{Kty: "EC", Crv: "P-256", X: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", Y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}
	// Built independently: members crv, kty, x, y in lexicographic order, no whitespace.
	want := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU","y":"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}`))
	if got := Thumbprint(j); got != base64.RawURLEncoding.EncodeToString(want[:]) {
		t.Fatalf("thumbprint = %s", got)
	}
}

func TestProvisionWritesOwnerOnlyPrivateKeyAndPublicTrustEntry(t *testing.T) {
	dir := t.TempDir()
	entry, err := Provision(dir, "steelmatter-service", []string{"SM-PERMIT+JWS"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "steelmatter-service.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("private key mode = %o; want 600", mode)
	}

	priv, err := ReadPrivateKey(filepath.Join(dir, "steelmatter-service.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := JWKFromPublic(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if Thumbprint(jwk) != entry.Kid || jwk != entry.JWK {
		t.Fatal("trust store entry does not match the private key")
	}
	if !entry.Synthetic || entry.Alg != "ES256" {
		t.Fatalf("entry = %+v", entry)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "trust-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"d"`, "PRIVATE KEY"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("trust store contains private material marker %s", forbidden)
		}
	}
}

func TestProvisionRefusesToReplaceExistingKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, "observer", testPurposes, fixedNow); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "observer.key.pem"))
	if _, err := Provision(dir, "observer", testPurposes, fixedNow); err == nil {
		t.Fatal("second Provision with the same name succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "observer.key.pem"))
	if string(before) != string(after) {
		t.Fatal("existing private key was modified")
	}
}

func TestProvisionRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", "../escape", "Upper", "a/b", strings.Repeat("a", 64)} {
		if _, err := Provision(t.TempDir(), name, testPurposes, fixedNow); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestLoadTrustStoreRejectsTamperedEntry(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, "owner", testPurposes, fixedNow); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trust-store.json")
	ts, err := LoadTrustStore(path)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk, _ := JWKFromPublic(&other.PublicKey)
	ts.Keys[0].JWK = jwk // kid no longer matches
	if err := ts.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustStore(path); err == nil {
		t.Fatal("tampered trust store accepted")
	}
}

func TestLoadTrustStoreRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust-store.json")
	if err := os.WriteFile(path, []byte(`{"trust_store_version":"1","keys":[],"jku":"https://example.invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustStore(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestProvisionRequiresPurpose(t *testing.T) {
	if _, err := Provision(t.TempDir(), "k", nil, fixedNow); err == nil {
		t.Fatal("key without purpose accepted")
	}
}

func TestPublicKeyRejectsPointOffCurve(t *testing.T) {
	priv, _ := Generate()
	j, _ := JWKFromPublic(&priv.PublicKey)
	if _, err := PublicKey(j); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	j.Y = j.X // almost certainly not on the curve
	if _, err := PublicKey(j); err == nil {
		t.Fatal("off-curve point accepted")
	}
}

func TestParseTrustStoreIsStrict(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, "owner", testPurposes, fixedNow); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(filepath.Join(dir, "trust-store.json"))
	if _, err := ParseTrustStore(good); err != nil {
		t.Fatalf("valid store rejected: %v", err)
	}
	ts, _ := ParseTrustStore(good)
	k := ts.Keys[0]
	entry := func(kid, name string) string {
		return fmt.Sprintf(`{"kid":%q,"name":%q,"alg":"ES256","synthetic":true,"purposes":["SM-MANDATE+JWS"],"created_at":"2026-10-12T09:00:00Z","jwk":{"kty":"EC","crv":"P-256","x":%q,"y":%q}}`,
			kid, name, k.JWK.X, k.JWK.Y)
	}
	for name, doc := range map[string]string{
		"duplicate top-level key": `{"trust_store_version":"1","trust_store_version":"1","keys":[]}`,
		"case-variant field":      `{"Trust_Store_Version":"1","keys":[]}`,
		"duplicate kid":           `{"trust_store_version":"1","keys":[` + entry(k.Kid, "a") + `,` + entry(k.Kid, "b") + `]}`,
		"case-variant key field":  `{"trust_store_version":"1","keys":[` + strings.Replace(entry(k.Kid, "a"), `"kid"`, `"KID"`, 1) + `]}`,
		"duplicate jwk member":    `{"trust_store_version":"1","keys":[` + strings.Replace(entry(k.Kid, "a"), `"kty":"EC"`, `"kty":"EC","kty":"EC"`, 1) + `]}`,
	} {
		if _, err := ParseTrustStore([]byte(doc)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReadPrivateKeyRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, "loose", testPurposes, fixedNow); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "loose.key.pem")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(path); err == nil {
		t.Fatal("world-readable private key accepted")
	}
}

func TestProvisionRejectsInvalidPurposes(t *testing.T) {
	for _, purposes := range [][]string{
		{"SM-ACTION+JWS"}, {""}, {"sm-permit+jws"}, {" SM-PERMIT+JWS"}, {"SM-PERMIT-JWS"}, {"SM-PERMIT+JWS", "SM-PERMIT+JWS"},
	} {
		if _, err := Provision(t.TempDir(), "k", purposes, fixedNow); err == nil {
			t.Errorf("purposes %q accepted", purposes)
		}
	}
	if _, err := Provision(t.TempDir(), "all", KnownPurposes, fixedNow); err != nil {
		t.Fatalf("all four known purposes rejected: %v", err)
	}
}

func TestRetireKeepsHistoricalValidity(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, "svc", []string{"SM-PERMIT+JWS"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trust-store.json")
	ts, _ := LoadTrustStore(path)
	if err := ts.Retire("svc", fixedNow.Add(-time.Hour)); err == nil {
		t.Fatal("retirement before creation accepted")
	}
	at := fixedNow.Add(time.Hour)
	if err := ts.Retire("svc", at); err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(path); err != nil {
		t.Fatal(err)
	}
	ts2, err := LoadTrustStore(path)
	if err != nil {
		t.Fatalf("store with not_after rejected: %v", err)
	}
	k := ts2.Keys[0]
	if k.RetiredAt(at.Add(-time.Second)) || !k.RetiredAt(at) || !k.RetiredAt(at.Add(time.Second)) {
		t.Fatal("retirement must take effect exactly at not_after")
	}
}
