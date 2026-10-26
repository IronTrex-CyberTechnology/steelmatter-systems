// Package failpoint provides named crash/pause points for recovery and race
// tests (T07, T08, T10, T13-T16, T27, T28) and worker failure isolation. Points are no-ops unless the
// binary is built with the "failpoints" build tag; release builds
// (scripts/package) never include them.
package failpoint

import "context"

// Named points. Each is placed at the exact step its name describes.
const (
	EvaluateBeforeReserveCommit = "evaluate.before_reserve_commit" // reservation written, transaction not committed
	EvaluateAfterReserve        = "evaluate.after_reserve"         // reservation committed, permit not yet signed
	EvaluateBeforePersist       = "evaluate.before_persist"        // signed outside the transaction, not yet persisted
	AdmissionAfterLocks         = "admission.after_locks"          // all admission locks held, before rechecks
	AdmissionAfterTargetInsert  = "admission.after_target_insert"  // order row inserted, transaction not committed
	AdmissionAfterCommit        = "admission.after_commit"         // committed, response not yet sent
	RevokeAfterLock             = "revoke.after_lock"              // mandate locked, before revocation writes
	CleanupAfterLocks           = "cleanup.after_locks"            // cleanup locks held, before rechecks
	AmendAfterLocks             = "amend.after_locks"              // amendment locks held, before rechecks
	OutboxAfterDeliver          = "outbox.after_deliver"           // event delivered, not yet marked delivered
	EvidenceBeforeSign          = "evidence.before_sign"           // pending execution record loaded, not yet signed
	ObserverBeforeReadback      = "observer.before_readback"       // observer key checked, readback not yet run
)

// Inject runs the action registered for name, if any. A returned error
// aborts the surrounding operation as if the process failed at that point.
func Inject(ctx context.Context, name string) error {
	return inject(ctx, name)
}

type keyCtx struct{}

// WithKey labels ctx with a business key (for example an instruction ID) so
// a test action can decide deterministically which requests to fail,
// independent of goroutine scheduling (T02 seed reproducibility).
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyCtx{}, key)
}

// KeyFrom returns the label set by WithKey, or "".
func KeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(keyCtx{}).(string)
	return k
}
