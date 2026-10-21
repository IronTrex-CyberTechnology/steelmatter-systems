package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/IronTrex-CyberTechnology/Steelmatter/core/keys"
)

// LoadKeyring reads dir/trust-store.json and every dir/<name>.key.pem that
// exists, returning the trust store and one signer per JWS typ. A private key
// must match its trust-store entry, and each typ may have only one signer.
func LoadKeyring(dir string) (*keys.TrustStore, map[string]Signer, error) {
	trust, err := keys.LoadTrustStore(filepath.Join(dir, "trust-store.json"))
	if err != nil {
		return nil, nil, err
	}
	signers := map[string]Signer{}
	for _, k := range trust.Keys {
		priv, err := keys.ReadPrivateKey(filepath.Join(dir, k.Name+".key.pem"))
		if errors.Is(err, os.ErrNotExist) {
			continue // verification-only key
		}
		if err != nil {
			return nil, nil, fmt.Errorf("key %s: %w", k.Name, err)
		}
		jwk, err := keys.JWKFromPublic(&priv.PublicKey)
		if err != nil || keys.Thumbprint(jwk) != k.Kid {
			return nil, nil, fmt.Errorf("key %s does not match its trust-store entry", k.Name)
		}
		if k.RetiredAt(time.Now()) {
			return nil, nil, fmt.Errorf("key %s was retired at %s; rotate to a current key before starting", k.Name, k.NotAfter.Format(time.RFC3339))
		}
		// The observer's key signs observations only, so the executor's keys
		// can never produce an observation and vice versa (I9; F-09).
		if k.Allows("SM-OBSERVATION+JWS") && len(k.Purposes) > 1 {
			return nil, nil, fmt.Errorf("key %s: an observation key must have no other purpose", k.Name)
		}
		for _, typ := range k.Purposes {
			if _, dup := signers[typ]; dup {
				return nil, nil, fmt.Errorf("more than one signing key for %s", typ)
			}
			signers[typ] = Signer{Kid: k.Kid, Key: priv}
		}
	}
	return trust, signers, nil
}

// SplitObserver separates the observation signer from the engine's signers.
func SplitObserver(all map[string]Signer) (engine map[string]Signer, observer Signer, ok bool) {
	engine = map[string]Signer{}
	for typ, s := range all {
		if typ == "SM-OBSERVATION+JWS" {
			observer, ok = s, true
			continue
		}
		engine[typ] = s
	}
	return engine, observer, ok
}
