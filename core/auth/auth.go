// Package auth authenticates synthetic principals (I1). A bearer token maps
// to exactly one enabled principal with a kind and a tenant; identity is
// never taken from request bodies.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Principal kinds.
const (
	Owner    = "OWNER"
	Issuer   = "ISSUER"
	Actor    = "ACTOR"
	Observer = "OBSERVER"
	Operator = "OPERATOR"
)

// Principal is an authenticated caller.
type Principal struct {
	ID     string
	Tenant string
	Kind   string
}

// ErrUnauthenticated means no valid credential was presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// NewToken returns a fresh synthetic token ("smt_" + 64 hex) and its hash.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = "smt_" + hex.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken returns the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticator resolves bearer tokens against steelmatter.principals.
type Authenticator struct {
	Pool *pgxpool.Pool
}

// FromHeader authenticates an Authorization header value. Database errors
// are returned as-is so callers fail closed with 503, not 401.
func (a *Authenticator) FromHeader(ctx context.Context, header string) (Principal, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || !strings.HasPrefix(token, "smt_") || len(token) != 68 {
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	err := a.Pool.QueryRow(ctx, `SELECT principal_id, tenant_id, kind FROM steelmatter.principals
		WHERE token_sha256 = $1 AND enabled`, HashToken(token)).Scan(&p.ID, &p.Tenant, &p.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	return p, err
}
