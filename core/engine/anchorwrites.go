package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
)

// UseAnchor connects the engine to the recovery anchor (D5 v3) and adopts
// its current write generation. From then on every dispatch records its
// intent in the anchor before committing, every destination result is
// recorded there, and dispatch is refused while writes are off or this
// engine's generation is old.
func (e *Engine) UseAnchor(ctx context.Context, c *anchor.Client) (anchor.WriteState, error) {
	e.Anchor = c
	return e.AdoptWriteState(ctx)
}

// AdoptWriteState loads the anchor's write generation into this engine. A
// process started before a restore keeps its old generation and is refused
// until it adopts the new one (at restart, or after the resume gate it ran).
func (e *Engine) AdoptWriteState(ctx context.Context) (anchor.WriteState, error) {
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return ws, err
	}
	e.anchorGen.Store(ws.Generation)
	return ws, nil
}

// WriteGeneration is the anchor generation this engine dispatches under.
func (e *Engine) WriteGeneration() int64 { return e.anchorGen.Load() }

// writesOn reports whether this engine may write new orders: writes are
// enabled in the anchor at this engine's generation. Without an anchor
// writes are always on. A read failure is an error: callers fail closed.
func (e *Engine) writesOn(ctx context.Context) (bool, anchor.WriteState, error) {
	if e.Anchor == nil {
		return true, anchor.WriteState{Enabled: true}, nil
	}
	ws, err := e.Anchor.WriteState(ctx)
	if err != nil {
		return false, ws, err
	}
	return ws.Enabled && ws.Generation == e.anchorGen.Load(), ws, nil
}

func (e *Engine) requireWrites(ctx context.Context) error {
	on, ws, err := e.writesOn(ctx)
	switch {
	case err != nil:
		return fault("SERVICE_UNAVAILABLE", "recovery anchor is unavailable; nothing was dispatched")
	case !on && !ws.Enabled:
		return fault("SERVICE_UNAVAILABLE", "writes are disabled (%s); nothing was dispatched", ws.Reason)
	case !on:
		return fault("SERVICE_UNAVAILABLE", "this process's write generation is old; restart it to adopt generation %d", ws.Generation)
	}
	return nil
}

// anchorFault maps an anchor append failure to a client-visible fault.
func anchorFault(err error) error {
	if errors.Is(err, anchor.ErrWritesDisabled) {
		return fault("SERVICE_UNAVAILABLE", "writes were disabled during dispatch; nothing was dispatched")
	}
	return fault("SERVICE_UNAVAILABLE", "recovery anchor is unavailable; nothing was dispatched")
}

func moveFor(status string) string {
	switch status {
	case DispatchCommitted:
		return "unresolved->committed"
	case DispatchFenced:
		return "unresolved->available"
	}
	return "none"
}

// anchorOutcome records a destination result and the resulting status in the
// anchor (mechanism step 2). It runs inside the engine transaction before
// commit, so the engine never records an outcome the anchor lacks.
func (e *Engine) anchorOutcome(ctx context.Context, rec anchor.OutcomeRecord) error {
	if e.Anchor == nil {
		return nil
	}
	if _, err := e.Anchor.Append(ctx, anchor.Outcome, rec.Tenant, rec.OpRef, rec, 0); err != nil {
		return fmt.Errorf("recovery anchor: %w", err)
	}
	return nil
}
