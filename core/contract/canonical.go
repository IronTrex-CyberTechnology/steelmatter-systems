package contract

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/gowebpki/jcs"
)

// Canonicalize returns the RFC 8785 (JCS) bytes of raw. Callers validate raw
// with Validate or ParseStrict first; the profile admits only strings,
// integers within ±(2^53-1), booleans, null, arrays and objects, for which
// JCS output is identical in Go and TypeScript.
func Canonicalize(raw []byte) ([]byte, error) {
	return jcs.Transform(raw)
}

// DigestContext is the domain-separation prefix for a payload digest.
func DigestContext(k Kind) string {
	return "steelmatter:" + string(k) + ":v1"
}

// Digest returns the lowercase hex SHA-256 of
// ASCII(DigestContext(kind)) || 0x0A || canonical.
// canonical must already be RFC 8785 bytes.
func Digest(k Kind, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(DigestContext(k)))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalDigest validates raw as kind, then returns its canonical bytes and digest.
func CanonicalDigest(k Kind, raw []byte) (canonical []byte, digest string, err error) {
	if _, verr := Validate(k, raw); verr != nil {
		return nil, "", verr
	}
	canonical, err = Canonicalize(raw)
	if err != nil {
		return nil, "", err
	}
	return canonical, Digest(k, canonical), nil
}
