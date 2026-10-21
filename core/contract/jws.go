package contract

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
)

// Signed payload kinds and their JWS typ values. Actions are digested and
// bound by the permit; they are not signed on their own.
var jwsTypes = map[Kind]string{
	KindMandate:     "SM-MANDATE+JWS",
	KindPermit:      "SM-PERMIT+JWS",
	KindExecution:   "SM-EXECUTION+JWS",
	KindObservation: "SM-OBSERVATION+JWS",
}

// TypeOf returns the JWS typ for a signed kind.
func TypeOf(k Kind) (string, bool) {
	t, ok := jwsTypes[k]
	return t, ok
}

// MaxJWSBytes bounds a compact JWS: a 1 MiB payload grows by 4/3 in base64url.
const MaxJWSBytes = MaxPayloadBytes*4/3 + 4096

var b64 = base64.RawURLEncoding.Strict()

// ProtectedHeader returns the pinned protected header bytes. Exactly these
// three members in this order with no whitespace; kid is a base64url
// thumbprint, so it needs no escaping.
func ProtectedHeader(k Kind, kid string) ([]byte, error) {
	typ, ok := jwsTypes[k]
	if !ok {
		return nil, fmt.Errorf("kind %q is not signed", k)
	}
	if !isBase64URL(kid) || kid == "" {
		return nil, errors.New("kid must be a base64url thumbprint")
	}
	return []byte(`{"alg":"ES256","kid":"` + kid + `","typ":"` + typ + `"}`), nil
}

// SigningInput returns ASCII(BASE64URL(header) || '.' || BASE64URL(payload)).
func SigningInput(k Kind, kid string, canonicalPayload []byte) (string, error) {
	h, err := ProtectedHeader(k, kid)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(h) + "." + b64.EncodeToString(canonicalPayload), nil
}

// Signer holds a trusted signing key.
type Signer struct {
	Kid string
	Key *ecdsa.PrivateKey
}

// Sign validates raw as kind, canonicalises it and returns a compact JWS.
// The output's signing input always equals SigningInput; this is asserted so
// a library change in header serialisation fails loudly instead of producing
// an unpinned header.
func Sign(k Kind, raw []byte, s Signer) (string, error) {
	typ, ok := jwsTypes[k]
	if !ok {
		return "", fmt.Errorf("kind %q is not signed", k)
	}
	if _, verr := Validate(k, raw); verr != nil {
		return "", verr
	}
	canonical, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	want, err := SigningInput(k, s.Kid, canonical)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: s.Key, KeyID: s.Kid}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)),
	)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(canonical)
	if err != nil {
		return "", err
	}
	compact, err := obj.CompactSerialize()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(compact, want+".") {
		return "", errors.New("JWS library produced an unpinned signing input")
	}
	return compact, nil
}

// Verified is a JWS that passed every check in Verify.
type Verified struct {
	Kind      Kind
	Kid       string
	Payload   []byte // canonical payload bytes
	Digest    string
	Value     any // typed payload
	SignedKey keys.TrustedKey
}

// Verify checks a compact JWS for the expected kind against the trust store.
// Check order (pinned, mirrored by the verifier):
//  1. size and structure: three segments of strict unpadded base64url → JWS_MALFORMED
//  2. header parses strictly as an object → JWS_MALFORMED
//  3. alg == "ES256" → JWS_ALG_UNSUPPORTED
//  4. only alg, kid, typ members (no crit, jku, jwk, x5u, x5c, b64, ...) → JWS_HEADER_UNSUPPORTED
//  5. typ equals the expected kind's typ → JWS_TYPE_MISMATCH
//  6. kid is in the trust store and allowed to sign typ → JWS_KEY_UNTRUSTED
//  7. header bytes equal the pinned ProtectedHeader → JWS_MALFORMED
//  8. ES256 signature verifies under that key → JWS_SIGNATURE_INVALID
//  9. payload passes Validate(kind) → that payload's code
//  10. payload bytes equal their RFC 8785 form → JWS_PAYLOAD_NOT_CANONICAL
//
// Verify establishes integrity and signer attribution only. Expiry, current
// usability and linkage are separate checks (I11).
func Verify(compact string, k Kind, trust *keys.TrustStore) (*Verified, *Error) {
	typ, ok := jwsTypes[k]
	if !ok {
		return nil, errf(CodeJWSTypeMismatch, "", "kind %q is not signed", k)
	}
	if len(compact) > MaxJWSBytes {
		return nil, errf(CodeJWSMalformed, "", "JWS exceeds %d bytes", MaxJWSBytes)
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, errf(CodeJWSMalformed, "", "compact JWS must have three segments")
	}
	var dec [3][]byte
	for i, p := range parts {
		if !isBase64URL(p) {
			return nil, errf(CodeJWSMalformed, "", "segment %d is not unpadded base64url", i)
		}
		b, err := b64.DecodeString(p)
		if err != nil {
			return nil, errf(CodeJWSMalformed, "", "segment %d: %v", i, err)
		}
		dec[i] = b
	}
	hv, perr := ParseStrict(dec[0])
	if perr != nil {
		return nil, errf(CodeJWSMalformed, "", "protected header: %s", perr.Detail)
	}
	header, ok := hv.(map[string]any)
	if !ok {
		return nil, errf(CodeJWSMalformed, "", "protected header is not an object")
	}
	if alg, _ := header["alg"].(string); alg != "ES256" {
		return nil, errf(CodeJWSAlgUnsupported, "", "alg must be ES256")
	}
	for _, name := range sortedKeys(header) {
		if name != "alg" && name != "kid" && name != "typ" {
			return nil, errf(CodeJWSHeaderUnsupported, "", "header member %q is not supported", name)
		}
	}
	if got, _ := header["typ"].(string); got != typ {
		return nil, errf(CodeJWSTypeMismatch, "", "typ must be %s", typ)
	}
	kid, _ := header["kid"].(string)
	tk, found := trust.Lookup(kid)
	if !found || !tk.Allows(typ) {
		return nil, errf(CodeJWSKeyUntrusted, "", "kid is not trusted for %s", typ)
	}
	pinned, err := ProtectedHeader(k, kid)
	if err != nil || !bytes.Equal(pinned, dec[0]) {
		return nil, errf(CodeJWSMalformed, "", "protected header is not in pinned form")
	}
	pub, err := keys.PublicKey(tk.JWK)
	if err != nil {
		return nil, errf(CodeJWSKeyUntrusted, "", "trusted key unusable: %v", err)
	}
	if len(dec[2]) != 64 {
		return nil, errf(CodeJWSSignatureInvalid, "", "ES256 signature must be 64 bytes")
	}
	obj, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return nil, errf(CodeJWSMalformed, "", "parse: %v", err)
	}
	payload, err := obj.Verify(pub)
	if err != nil {
		return nil, errf(CodeJWSSignatureInvalid, "", "signature does not verify")
	}
	value, verr := Validate(k, payload)
	if verr != nil {
		return nil, verr
	}
	canonical, err := Canonicalize(payload)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, errf(CodeJWSPayloadNotCanonical, "", "payload is not RFC 8785 canonical")
	}
	return &Verified{Kind: k, Kid: kid, Payload: payload, Digest: Digest(k, payload), Value: value, SignedKey: tk}, nil
}

func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
