// Package engine implements the R0 transactions (I03-I06): mandates,
// trusted instructions, amendments, reservations, signed permits, atomic
// admission, cleanup, recovery and the outbox.
//
// Every mutation locks in the global order (docs/contracts/I02-contract.md §8):
//
//	mandates → instructions → action_revisions → issuances → target.suppliers
//
// and rechecks time with clock_timestamp() after the locks. No external call
// (including signing) happens inside a mutation transaction.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/contract"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/keys"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/observer"
)

// Config holds engine settings.
type Config struct {
	// Audience is this destination adapter's audience. Admission rejects
	// permits and actions for any other audience (T06).
	Audience string
	// AdapterID names the synthetic target in results.
	AdapterID string
	// PermitTTL is the permit lifetime, at most 5 minutes; whole seconds.
	PermitTTL time.Duration
	// MaxClockSkew is the allowed app-vs-database clock difference; larger
	// skew blocks issuance and admission (CLOCK_UNHEALTHY).
	MaxClockSkew time.Duration
	// MaxAttempts bounds database retries on serialization/deadlock errors.
	MaxAttempts int
	// RecoveryAge is how old an unsigned issuance must be before the recovery
	// worker signs it (avoids racing an in-flight evaluation).
	RecoveryAge time.Duration
	// Version is the build version reported by the adapter assurance endpoint.
	Version string
}

// DefaultConfig returns the R0 defaults.
func DefaultConfig() Config {
	return Config{
		Audience:     "synthetic-procurement",
		AdapterID:    "synthetic-procurement",
		PermitTTL:    5 * time.Minute,
		MaxClockSkew: 2 * time.Second,
		MaxAttempts:  3,
		RecoveryAge:  10 * time.Second,
		Version:      "dev",
	}
}

// Engine is the transactional core. It uses only the executor database role.
type Engine struct {
	Pool    *pgxpool.Pool
	Trust   *keys.TrustStore
	Signers map[string]contract.Signer // JWS typ → signer
	Cfg     Config
	// Now is the application clock, compared with the database clock.
	Now func() time.Time
	// Observer performs read-only readback (nil disables observation).
	Observer *observer.Observer
}

// New validates the configuration and returns an Engine.
func New(pool *pgxpool.Pool, trust *keys.TrustStore, signers map[string]contract.Signer, cfg Config) (*Engine, error) {
	if cfg.PermitTTL <= 0 || cfg.PermitTTL > contract.MaxPermitLifetime || cfg.PermitTTL%time.Second != 0 {
		return nil, fmt.Errorf("permit TTL %s must be whole seconds in (0, 5m]", cfg.PermitTTL)
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 3 {
		return nil, errors.New("max attempts must be 1..3")
	}
	if _, ok := signers["SM-OBSERVATION+JWS"]; ok {
		return nil, errors.New("the observation key must not be given to the engine (use contract.SplitObserver)")
	}
	for _, typ := range []string{"SM-MANDATE+JWS", "SM-PERMIT+JWS", "SM-EXECUTION+JWS"} {
		s, ok := signers[typ]
		if !ok {
			return nil, fmt.Errorf("no signing key for %s", typ)
		}
		if k, ok := trust.Lookup(s.Kid); !ok || !k.Allows(typ) {
			return nil, fmt.Errorf("signing key for %s is not trusted for it", typ)
		}
	}
	return &Engine{Pool: pool, Trust: trust, Signers: signers, Cfg: cfg, Now: time.Now}, nil
}

// Fault is a client-visible failure with a stable code from core/httpapi.
type Fault struct {
	Code        string
	Message     string
	OperationID string
}

func (f *Fault) Error() string { return f.Code + ": " + f.Message }

func fault(code, format string, a ...any) *Fault {
	return &Fault{Code: code, Message: fmt.Sprintf(format, a...)}
}

// AsFault returns the Fault in err, if any.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	ok := errors.As(err, &f)
	return f, ok
}

// newID returns prefix-<24 hex>, matching the Identifier pattern.
func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// RetryBackoff is the wait before attempt 2 and attempt 3 of a transaction
// that failed with a serialization failure (40001) or deadlock (40P01).
// Specified in I02 §8; pinned by TestRetryBackoffSpecification.
var RetryBackoff = []time.Duration{10 * time.Millisecond, 50 * time.Millisecond}

// TxBudget bounds one inTx call including retries, so the whole request
// stays under the HTTP write timeout. Each attempt is also bounded to 30 s
// by PostgreSQL's transaction_timeout (document 02 limits).
const TxBudget = 30 * time.Second

// inTx runs fn in a READ COMMITTED transaction. Serialization failures and
// deadlocks are retried up to Cfg.MaxAttempts in total; fn must reuse any
// business IDs generated before the call. A Fault returned by fn rolls back
// and is returned unchanged. opID must be an operation ID that already
// exists (or "" if none), because it is returned for lookup.
func (e *Engine) inTx(ctx context.Context, opID string, fn func(tx pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, TxBudget)
	defer cancel()
	backoff := RetryBackoff
	var last error
	for attempt := 0; attempt < e.Cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return classify(ctx.Err())
			case <-time.After(backoff[attempt-1]):
			}
		}
		err := pgx.BeginTxFunc(ctx, e.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			// Bound the whole transaction, each statement and idle time.
			if _, err := tx.Exec(ctx, `SET LOCAL transaction_timeout = '30s'; SET LOCAL statement_timeout = '30s'; SET LOCAL idle_in_transaction_session_timeout = '30s'`); err != nil {
				return err
			}
			return fn(tx)
		})
		if err == nil {
			return nil
		}
		if _, ok := AsFault(err); ok {
			return err
		}
		if !retryable(err) {
			return classify(err)
		}
		last = err
	}
	slog.Warn("database retries exhausted", "sqlstate", sqlState(last), "operation_id", opID)
	f := fault("RETRIES_EXHAUSTED", "database retries exhausted")
	f.OperationID = opID
	return f
}

func retryable(err error) bool {
	s := sqlState(err)
	return s == "40001" || s == "40P01"
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func isUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

// unavailableStates are SQLSTATE classes/codes that mean the authoritative
// database could not serve the request: connection exceptions (08),
// insufficient resources (53), operator intervention (57: cancel, timeout,
// shutdown) and transaction/idle timeouts (25P03, 25P04).
func unavailableState(s string) bool {
	return strings.HasPrefix(s, "08") || strings.HasPrefix(s, "53") || strings.HasPrefix(s, "57") || s == "25P03" || s == "25P04"
}

// classify maps a non-Fault error to a client-visible Fault (F-14):
//   - connectivity, timeouts and cancellation → SERVICE_UNAVAILABLE (retryable).
//     Nothing is allowed from cache; callers retry with the same identities (T27).
//   - anything else (constraint violations, data errors, bugs) → INTERNAL_ERROR,
//     not retryable, logged with its SQLSTATE and constraint so an invariant
//     breach can never look like a transient outage.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if f, ok := AsFault(err); ok && f != errSourceRace {
		return f
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if unavailableState(pgErr.Code) {
			slog.Warn("database unavailable", "sqlstate", pgErr.Code, "err", pgErr.Message)
			return &Fault{Code: "SERVICE_UNAVAILABLE", Message: "authoritative database unavailable"}
		}
		slog.Error("unexpected database error", "sqlstate", pgErr.Code, "constraint", pgErr.ConstraintName,
			"table", pgErr.TableName, "err", pgErr.Message)
		return &Fault{Code: "INTERNAL_ERROR", Message: "unexpected database error"}
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isNetErr(err) || pgconn.SafeToRetry(err) || errors.Is(err, errSourceRace) {
		slog.Warn("database unavailable", "err", err)
		return &Fault{Code: "SERVICE_UNAVAILABLE", Message: "authoritative database unavailable"}
	}
	if strings.Contains(err.Error(), "conn closed") || strings.Contains(err.Error(), "failed to deallocate") {
		slog.Warn("database unavailable", "err", err)
		return &Fault{Code: "SERVICE_UNAVAILABLE", Message: "authoritative database unavailable"}
	}
	slog.Error("unexpected internal error", "err", err)
	return &Fault{Code: "INTERNAL_ERROR", Message: "unexpected internal error"}
}

func isNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}

// unavailable is kept for call sites outside transactions; it classifies.
func unavailable(err error) error { return classify(err) }

// checkClock compares the database clock with the application clock.
func (e *Engine) checkClock(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var db time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&db); err != nil {
		return err
	}
	if skew := math.Abs(float64(e.Now().Sub(db))); skew > float64(e.Cfg.MaxClockSkew) {
		slog.Warn("clock unhealthy", "skew", time.Duration(skew), "limit", e.Cfg.MaxClockSkew)
		return fault("CLOCK_UNHEALTHY", "clock health check failed")
	}
	return nil
}

// Ready reports whether the engine can admit work: the database answers and
// the clocks agree (used by GET /v1/ready).
func (e *Engine) Ready(ctx context.Context) error {
	return e.checkClock(ctx, e.Pool)
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// signer returns the signing key for typ, refusing a key that has been
// retired since startup (I02 checklist item 5).
func (e *Engine) signer(typ string) (contract.Signer, error) {
	s, ok := e.Signers[typ]
	if !ok {
		return contract.Signer{}, fault("SERVICE_UNAVAILABLE", "no signing key for %s", typ)
	}
	if k, ok := e.Trust.Lookup(s.Kid); !ok || k.RetiredAt(e.Now()) {
		slog.Error("signing key retired or untrusted", "typ", typ, "kid", s.Kid)
		return contract.Signer{}, fault("SERVICE_UNAVAILABLE", "no current signing key for %s", typ)
	}
	return s, nil
}
