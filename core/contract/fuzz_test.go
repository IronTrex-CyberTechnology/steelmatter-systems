package contract

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Exercise arbitrary bytes against the strict parser. Any accepted value must
// also be valid standard JSON and have stable canonical bytes on re-parsing.
func FuzzStrictJSONCanonicalRoundTrip(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"a":1,"b":[true,null,"text"]}`), []byte(`{"a":1,"a":2}`),
		[]byte(`"\uD83D\uDE00"`), []byte(`"\u+041"`), []byte(`"\uD800"`),
		[]byte(`-0`), []byte(`1e3`), []byte(`9007199254740992`),
		[]byte(`{"__proto__":{"approved":true}}`), {0xef, 0xbb, 0xbf, '{', '}'},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, parseErr := ParseStrict(raw)
		if parseErr != nil {
			return
		}
		if !json.Valid(raw) {
			t.Fatalf("strict parser accepted invalid JSON: %q", raw)
		}
		canonical, err := Canonicalize(raw)
		if err != nil {
			t.Fatalf("accepted JSON cannot be canonicalized: %v", err)
		}
		if _, err := ParseStrict(canonical); err != nil {
			t.Fatalf("canonical output is not strict JSON: %v", err)
		}
		again, err := Canonicalize(canonical)
		if err != nil || !bytes.Equal(canonical, again) {
			t.Fatalf("canonicalization is not stable: %v", err)
		}
	})
}
