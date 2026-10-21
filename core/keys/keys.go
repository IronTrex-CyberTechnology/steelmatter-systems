// Package keys generates synthetic ES256 development keys and maintains the
// local trust store of public keys (document 03: allowed key IDs come from the
// configured local trust store; no remote key fetch).
//
// Private keys are PKCS#8 PEM files with mode 0600 and never enter the trust
// store. Key IDs are RFC 7638 JWK thumbprints of the public key.
package keys

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Algorithm     = "ES256"
	TrustStoreVer = "1"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// PublicJWK is the public half of a P-256 key (RFC 7517/7518).
type PublicJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// TrustedKey is one trust-store entry.
type TrustedKey struct {
	Kid       string `json:"kid"`
	Name      string `json:"name"`
	Alg       string `json:"alg"`
	Synthetic bool   `json:"synthetic"`
	// Purposes lists the JWS typ values this key may sign, for example
	// "SM-PERMIT+JWS". A signature with any other typ is untrusted.
	Purposes  []string  `json:"purposes"`
	CreatedAt time.Time `json:"created_at"`
	// NotAfter, if set, retires the key from that instant: it may no longer
	// sign or authorise anything, but records it signed before NotAfter remain
	// verifiable historical evidence (I02 checklist item 5).
	NotAfter *time.Time `json:"not_after,omitempty"`
	JWK      PublicJWK  `json:"jwk"`
}

// KnownPurposes are the only valid purposes: the four signed JWS types
// (I02 §5; CLR-03). core/contract asserts this list matches its own types.
var KnownPurposes = []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS", "SM-OBSERVATION+JWS"}

// checkPurposes requires a non-empty list of distinct, exactly spelled known
// purposes. Unknown values, other letter case, whitespace and repeats are
// rejected rather than silently ignored (CLR-03).
func checkPurposes(purposes []string) error {
	if len(purposes) == 0 {
		return errors.New("at least one purpose is required")
	}
	seen := map[string]bool{}
	for _, p := range purposes {
		known := false
		for _, k := range KnownPurposes {
			known = known || p == k
		}
		if !known {
			return fmt.Errorf("unknown purpose %q (allowed: %s)", p, strings.Join(KnownPurposes, ", "))
		}
		if seen[p] {
			return fmt.Errorf("purpose %q is repeated", p)
		}
		seen[p] = true
	}
	return nil
}

// TrustStore lists trusted public keys. Signature purposes per payload type
// are pinned in I02; this file only establishes which keys exist.
type TrustStore struct {
	Version string       `json:"trust_store_version"`
	Keys    []TrustedKey `json:"keys"`
}

// Generate creates a new P-256 key pair.
func Generate() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// JWKFromPublic encodes a P-256 public key as a JWK.
func JWKFromPublic(pub *ecdsa.PublicKey) (PublicJWK, error) {
	if pub.Curve != elliptic.P256() {
		return PublicJWK{}, errors.New("only P-256 keys are supported")
	}
	raw, err := pub.Bytes() // uncompressed SEC 1: 0x04 || X || Y
	if err != nil {
		return PublicJWK{}, err
	}
	if len(raw) != 65 || raw[0] != 4 {
		return PublicJWK{}, errors.New("unexpected P-256 public key encoding")
	}
	enc := base64.RawURLEncoding
	return PublicJWK{Kty: "EC", Crv: "P-256", X: enc.EncodeToString(raw[1:33]), Y: enc.EncodeToString(raw[33:])}, nil
}

// PublicKey decodes a P-256 JWK, rejecting points not on the curve.
func PublicKey(j PublicJWK) (*ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, errors.New("only EC P-256 keys are supported")
	}
	x, errX := base64.RawURLEncoding.Strict().DecodeString(j.X)
	y, errY := base64.RawURLEncoding.Strict().DecodeString(j.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("invalid P-256 coordinates")
	}
	raw := append([]byte{4}, append(x, y...)...)
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
}

// Allows reports whether the key may sign the given JWS typ.
// RetiredAt reports whether the key is retired at t (t >= NotAfter).
func (k TrustedKey) RetiredAt(t time.Time) bool {
	return k.NotAfter != nil && !t.Before(*k.NotAfter)
}

// Retire sets NotAfter for the named key.
func (ts *TrustStore) Retire(name string, at time.Time) error {
	for i := range ts.Keys {
		if ts.Keys[i].Name == name {
			at = at.UTC()
			if !at.After(ts.Keys[i].CreatedAt) {
				return fmt.Errorf("key %s: not_after must be after created_at", name)
			}
			ts.Keys[i].NotAfter = &at
			return nil
		}
	}
	return fmt.Errorf("no key named %q", name)
}

func (k TrustedKey) Allows(typ string) bool {
	for _, p := range k.Purposes {
		if p == typ {
			return true
		}
	}
	return false
}

// Lookup returns the trusted key with kid.
func (ts *TrustStore) Lookup(kid string) (TrustedKey, bool) {
	for _, k := range ts.Keys {
		if k.Kid == kid {
			return k, true
		}
	}
	return TrustedKey{}, false
}

// Thumbprint returns the RFC 7638 SHA-256 thumbprint, base64url without padding.
// The required members are serialised in lexicographic order with no whitespace.
func Thumbprint(j PublicJWK) string {
	canonical := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q,"y":%q}`, j.Crv, j.Kty, j.X, j.Y)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// WritePrivateKey writes key as PKCS#8 PEM with mode 0600. It refuses to
// overwrite an existing file.
func WritePrivateKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadPrivateKey loads a PKCS#8 PEM P-256 private key. On POSIX systems the
// file must not be readable or writable by group or others (F-24).
func ReadPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("private key %s has mode %o; it must be 0600 or stricter", path, info.Mode().Perm())
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("expected a PKCS#8 PRIVATE KEY PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("expected a P-256 ECDSA private key")
	}
	return ec, nil
}

// LoadTrustStore reads a trust store; a missing file yields an empty store.
func LoadTrustStore(path string) (*TrustStore, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &TrustStore{Version: TrustStoreVer}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseTrustStore(b)
}

// ParseTrustStore strictly decodes and checks a trust store document: known
// fields only, ES256, kid equal to the RFC 7638 thumbprint, at least one
// purpose and a valid P-256 point for every key.
func ParseTrustStore(b []byte) (*TrustStore, error) {
	// encoding/json matches field names case-insensitively and keeps the last
	// duplicate key, so check the exact shape first (F-24).
	if err := strictShape(b); err != nil {
		return nil, fmt.Errorf("parse trust store: %w", err)
	}
	var ts TrustStore
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ts); err != nil {
		return nil, fmt.Errorf("parse trust store: %w", err)
	}
	kids, names := map[string]bool{}, map[string]bool{}
	for _, k := range ts.Keys {
		if kids[k.Kid] || names[k.Name] {
			return nil, fmt.Errorf("trust store repeats kid %q or name %q", k.Kid, k.Name)
		}
		kids[k.Kid], names[k.Name] = true, true
	}
	if ts.Version != TrustStoreVer {
		return nil, fmt.Errorf("unsupported trust store version %q", ts.Version)
	}
	for _, k := range ts.Keys {
		if k.Alg != Algorithm || Thumbprint(k.JWK) != k.Kid {
			return nil, fmt.Errorf("trust store entry %q is inconsistent", k.Kid)
		}
		if err := checkPurposes(k.Purposes); err != nil {
			return nil, fmt.Errorf("trust store entry %q: %w", k.Kid, err)
		}
		if k.NotAfter != nil && !k.NotAfter.After(k.CreatedAt) {
			return nil, fmt.Errorf("trust store entry %q: not_after must be after created_at", k.Kid)
		}
		if _, err := PublicKey(k.JWK); err != nil {
			return nil, fmt.Errorf("trust store entry %q: %w", k.Kid, err)
		}
	}
	return &ts, nil
}

// Save writes the store atomically, sorted by name then kid.
func (ts *TrustStore) Save(path string) error {
	sort.Slice(ts.Keys, func(i, j int) bool {
		if ts.Keys[i].Name != ts.Keys[j].Name {
			return ts.Keys[i].Name < ts.Keys[j].Name
		}
		return ts.Keys[i].Kid < ts.Keys[j].Kid
	})
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Add appends a key; duplicate names or kids are rejected.
func (ts *TrustStore) Add(k TrustedKey) error {
	for _, e := range ts.Keys {
		if e.Kid == k.Kid || e.Name == k.Name {
			return fmt.Errorf("trust store already has key %q (%s)", e.Name, e.Kid)
		}
	}
	ts.Keys = append(ts.Keys, k)
	return nil
}

// Provision generates a named synthetic key under dir: dir/<name>.key.pem and
// an entry in dir/trust-store.json. It refuses to replace an existing key.
func Provision(dir, name string, purposes []string, now time.Time) (TrustedKey, error) {
	if !namePattern.MatchString(name) {
		return TrustedKey{}, fmt.Errorf("key name %q must match %s", name, namePattern)
	}
	if err := checkPurposes(purposes); err != nil {
		return TrustedKey{}, err
	}
	storePath := filepath.Join(dir, "trust-store.json")
	ts, err := LoadTrustStore(storePath)
	if err != nil {
		return TrustedKey{}, err
	}
	for _, e := range ts.Keys {
		if e.Name == name {
			return TrustedKey{}, fmt.Errorf("key %q already exists", name)
		}
	}
	priv, err := Generate()
	if err != nil {
		return TrustedKey{}, err
	}
	jwk, err := JWKFromPublic(&priv.PublicKey)
	if err != nil {
		return TrustedKey{}, err
	}
	entry := TrustedKey{Kid: Thumbprint(jwk), Name: name, Alg: Algorithm, Synthetic: true, Purposes: append([]string(nil), purposes...), CreatedAt: now.UTC().Truncate(time.Second), JWK: jwk}
	if err := WritePrivateKey(filepath.Join(dir, name+".key.pem"), priv); err != nil {
		return TrustedKey{}, err
	}
	if err := ts.Add(entry); err != nil {
		return TrustedKey{}, err
	}
	return entry, ts.Save(storePath)
}

var (
	storeFields = []string{"keys", "trust_store_version"}
	keyFields   = []string{"alg", "created_at", "jwk", "kid", "name", "purposes", "synthetic"}
	jwkFields   = []string{"crv", "kty", "x", "y"}
)

// strictShape rejects duplicate object keys anywhere and requires the exact
// (case-sensitive) field sets of a trust store, its keys and their JWKs.
func strictShape(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := noDuplicateKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected content after the trust store")
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(b, &store); err != nil {
		return err
	}
	if err := exactFields(store, storeFields, "trust store"); err != nil {
		return err
	}
	if err := jsonKind(store["trust_store_version"], "string"); err != nil {
		return fmt.Errorf("trust_store_version: %w", err)
	}
	if err := jsonKind(store["keys"], "array"); err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(store["keys"], &entries); err != nil {
		return err
	}
	for _, k := range entries {
		if k == nil {
			return errors.New("trust store key must be an object")
		}
		if err := exactFields(k, keyFields, "trust store key", "not_after"); err != nil {
			return err
		}
		if raw, ok := k["not_after"]; ok {
			if err := jsonKind(raw, "string"); err != nil {
				return fmt.Errorf("trust store key not_after: %w", err)
			}
			var na string
			_ = json.Unmarshal(raw, &na)
			if !ValidRFC3339(na) {
				return fmt.Errorf("trust store key not_after %q is not a valid RFC 3339 date-time", na)
			}
		}
		// encoding/json turns null into a zero value without error, so check
		// the exact JSON type of every value first (QA-D1-003).
		for _, f := range []string{"kid", "name", "alg", "created_at"} {
			if err := jsonKind(k[f], "string"); err != nil {
				return fmt.Errorf("trust store key %s: %w", f, err)
			}
		}
		if err := jsonKind(k["synthetic"], "bool"); err != nil {
			return fmt.Errorf("trust store key synthetic: %w", err)
		}
		var purposes []json.RawMessage
		if err := jsonKind(k["purposes"], "array"); err != nil {
			return fmt.Errorf("trust store key purposes: %w", err)
		}
		if err := json.Unmarshal(k["purposes"], &purposes); err != nil {
			return err
		}
		for _, p := range purposes {
			if err := jsonKind(p, "string"); err != nil {
				return fmt.Errorf("trust store key purpose: %w", err)
			}
		}
		var created string
		_ = json.Unmarshal(k["created_at"], &created)
		if !ValidRFC3339(created) {
			return fmt.Errorf("trust store key created_at %q is not a valid RFC 3339 date-time", created)
		}
		if err := jsonKind(k["jwk"], "object"); err != nil {
			return fmt.Errorf("trust store key jwk: %w", err)
		}
		var jwk map[string]json.RawMessage
		if err := json.Unmarshal(k["jwk"], &jwk); err != nil {
			return err
		}
		if err := exactFields(jwk, jwkFields, "trust store jwk"); err != nil {
			return err
		}
		for _, f := range jwkFields {
			if err := jsonKind(jwk[f], "string"); err != nil {
				return fmt.Errorf("trust store jwk %s: %w", f, err)
			}
		}
	}
	return nil
}

// jsonKind requires raw to be a JSON value of the given kind; null never
// matches.
func jsonKind(raw json.RawMessage, kind string) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	ok := false
	switch kind {
	case "string":
		_, ok = v.(string)
	case "bool":
		_, ok = v.(bool)
	case "array":
		_, ok = v.([]any)
	case "object":
		_, ok = v.(map[string]any)
	}
	if !ok {
		return fmt.Errorf("must be a JSON %s, got %s", kind, strings.TrimSpace(string(raw)))
	}
	return nil
}

var rfc3339 = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d+)?(Z|[+-](\d{2}):(\d{2}))$`)

// ValidRFC3339 accepts an RFC 3339 date-time with real calendar values and a
// zone offset within ±23:59. The TypeScript verifier applies the same rule
// (packages/verifier/src/trust.ts), pinned by fixtures/vectors/truststore-v1.json.
func ValidRFC3339(s string) bool {
	m := rfc3339.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	y, mo, d, h, mi, se := n(1), n(2), n(3), n(4), n(5), n(6)
	if mo < 1 || mo > 12 || d < 1 || h > 23 || mi > 59 || se > 59 {
		return false
	}
	days := [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[mo-1]
	if mo == 2 && (y%4 == 0 && y%100 != 0 || y%400 == 0) {
		days = 29
	}
	if d > days {
		return false
	}
	if m[9] != "" && (n(9) > 23 || n(10) > 59) {
		return false
	}
	return true
}

func exactFields(m map[string]json.RawMessage, want []string, what string, optional ...string) error {
	for _, f := range optional {
		if _, ok := m[f]; ok {
			rest := map[string]json.RawMessage{}
			for k, v := range m {
				if k != f {
					rest[k] = v
				}
			}
			m = rest
		}
	}
	if len(m) != len(want) {
		return fmt.Errorf("%s must have exactly the fields %v", what, want)
	}
	for _, f := range want {
		if _, ok := m[f]; !ok {
			return fmt.Errorf("%s must have exactly the fields %v", what, want)
		}
	}
	return nil
}

func noDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("duplicate key %q", k)
			}
			seen[k] = true
			if err := noDuplicateKeys(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := noDuplicateKeys(dec); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}
