package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One active backend per deployment (R1, Aakhil 30 Oct 2026, NEW-SDET-01).
//
// serve takes a session-level advisory lock in the engine database and in
// the recovery anchor before it touches the anchor's write switch. A second
// backend for the same deployment finds a lock held and refuses to start
// without changing anything, so the running backend keeps its write
// generation and carries on. The locks live on dedicated connections; if
// either connection is lost, Lost reports it and serve stops, because
// another backend may then start.

// instanceLockKey is distinct from recoveryLockKey.
const instanceLockKey int64 = 0x5354_4d52_3149_4e53 // "STMR1INS"

// ErrAnotherInstance means another backend holds the deployment lock.
var ErrAnotherInstance = errors.New("another Steelmatter backend is already active for this deployment; R1 runs one backend per deployment")

// Instance holds the single-backend locks.
type Instance struct {
	conns []*pgx.Conn
	lost  chan error
	stop  chan struct{}
}

// AcquireInstance takes the single-backend lock in each pool (the engine
// database and, with R1, the recovery anchor). It never waits: a held lock
// returns ErrAnotherInstance with nothing changed.
func AcquireInstance(ctx context.Context, pools map[string]*pgxpool.Pool) (*Instance, error) {
	in := &Instance{lost: make(chan error, 1), stop: make(chan struct{})}
	for name, p := range pools {
		pc, err := p.Acquire(ctx)
		if err != nil {
			in.Close()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		conn := pc.Hijack() // kept out of the pool for the life of the process
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, instanceLockKey).Scan(&ok); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			in.Close()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !ok {
			_ = conn.Close(context.WithoutCancel(ctx))
			in.Close()
			return nil, fmt.Errorf("%w (lock held in the %s)", ErrAnotherInstance, name)
		}
		in.conns = append(in.conns, conn)
	}
	go in.watch(2 * time.Second)
	return in, nil
}

// Lost receives an error once a lock connection fails.
func (in *Instance) Lost() <-chan error { return in.lost }

func (in *Instance) watch(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-in.stop:
			return
		case <-t.C:
			for _, c := range in.conns {
				ctx, cancel := context.WithTimeout(context.Background(), every)
				err := c.Ping(ctx)
				cancel()
				if err != nil {
					select {
					case in.lost <- fmt.Errorf("single-backend lock connection lost: %w", err):
					default:
					}
					return
				}
			}
		}
	}
}

// Close releases the locks.
func (in *Instance) Close() {
	select {
	case <-in.stop:
		return
	default:
		close(in.stop)
	}
	for _, c := range in.conns {
		_ = c.Close(context.Background())
	}
}

// CheckNotOwner refuses a database connection whose role is a superuser or
// owns (directly or through membership) the given schemas or any table in
// them (decision 7, Aakhil 30 Oct 2026): the running backend never uses
// owner credentials, so database-owner changes stay an administrator action.
func CheckNotOwner(ctx context.Context, pool *pgxpool.Pool, what string, schemas ...string) error {
	var role string
	var owner bool
	err := pool.QueryRow(ctx, `SELECT current_user, r.rolsuper OR r.rolbypassrls
		OR EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = ANY($1) AND pg_has_role(current_user, n.nspowner, 'USAGE'))
		OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		           WHERE n.nspname = ANY($1) AND pg_has_role(current_user, c.relowner, 'USAGE'))
		FROM pg_roles r WHERE r.rolname = current_user`, schemas).Scan(&role, &owner)
	if err != nil {
		return fmt.Errorf("%s role check: %w", what, err)
	}
	if owner {
		return fmt.Errorf("%s connects as %q, which owns the %v schema or is a superuser; serve must use the restricted runtime role, never owner credentials", what, role, schemas)
	}
	return nil
}
