//go:build integration

package anchor_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/anchor"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/connector"
	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/dbtest"
)

func intent(op string) anchor.IntentRecord {
	return anchor.IntentRecord{Tenant: "t", OpRef: op, MandateID: "m", AmountMinor: 100, Currency: "INR", PermitDigest: "d"}
}

// Records are append-only for every role and hash-chained; a repeated intent
// with the same content is idempotent, a different one is refused.
func TestAnchorAppendOnlyAndChained(t *testing.T) {
	h := dbtest.NewAnchor(t)
	ctx := context.Background()
	c := &anchor.Client{Pool: h.Engine}
	s1, err := c.Append(ctx, anchor.DispatchIntent, "t", "op-1", intent("op-1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if s2, err := c.Append(ctx, anchor.DispatchIntent, "t", "op-1", intent("op-1"), 1); err != nil || s2 != s1 {
		t.Fatalf("repeat: %d %v", s2, err)
	}
	other := intent("op-1")
	other.AmountMinor = 200
	if _, err := c.Append(ctx, anchor.DispatchIntent, "t", "op-1", other, 1); err == nil {
		t.Fatal("a different intent for the same op_ref must be refused")
	}
	if _, err := c.Append(ctx, anchor.Outcome, "t", "op-1", anchor.OutcomeRecord{OpRef: "op-1", Status: "COMMITTED"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := anchor.Verify(ctx, h.Engine); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE anchor.engine_records SET payload = '{}'`, `DELETE FROM anchor.engine_records`, `TRUNCATE anchor.engine_records`,
		`INSERT INTO anchor.engine_records VALUES (99, 'OUTCOME', 't', 'x', '{}', '\x00', '\x00', now())`,
	} {
		if _, err := h.Engine.Exec(ctx, sql); err == nil {
			t.Fatalf("engine role could run %s", sql)
		}
		if _, err := h.Dest.Exec(ctx, sql); err == nil {
			t.Fatalf("destination role could run %s", sql)
		}
	}
	for _, sql := range []string{`UPDATE anchor.engine_records SET payload = '{}'`, `DELETE FROM anchor.engine_records`} {
		if _, err := h.Admin.Exec(ctx, sql); err == nil {
			t.Fatalf("even the owner must not change records: %s", sql)
		}
	}
	// A tamperer who disables triggers is caught by the hash chain.
	tx, _ := h.Admin.Begin(ctx)
	_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	if _, err := tx.Exec(ctx, `UPDATE anchor.engine_records SET payload = jsonb_set(payload, '{amount_minor}', '1') WHERE seq = 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := anchor.Verify(ctx, h.Engine); err == nil {
		t.Fatal("tampering went undetected")
	}
}

// Role separation: the engine cannot write destination records and the
// module cannot write engine records; the observer only reads.
func TestAnchorRoleSeparation(t *testing.T) {
	h := dbtest.NewAnchor(t)
	ctx := context.Background()
	if _, err := h.Engine.Exec(ctx, `SELECT anchor.append_destination('BLOCK', 'd', 'op', '{}')`); err == nil {
		t.Fatal("engine wrote a destination record")
	}
	if _, err := h.Dest.Exec(ctx, `SELECT anchor.append_engine('OUTCOME', 't', 'op', '{}', NULL)`); err == nil {
		t.Fatal("module wrote an engine record")
	}
	if _, err := h.Dest.Exec(ctx, `SELECT count(*) FROM anchor.engine_records`); err == nil {
		t.Fatal("module read engine records")
	}
	if _, err := h.Observer.Exec(ctx, `SELECT anchor.append_destination('BLOCK', 'd', 'op', '{}')`); err == nil {
		t.Fatal("observer wrote")
	}
	if _, err := h.Observer.Exec(ctx, `SELECT count(*) FROM anchor.destination_records`); err != nil {
		t.Fatalf("observer cannot read the destination stream: %v", err)
	}
	ds := &anchor.DestStream{Pool: h.Dest, Destination: "odoo"}
	if err := ds.RecordCreateIntent(ctx, "op", "d"); err != nil {
		t.Fatal(err)
	}
	if err := ds.RecordCreateDone(ctx, "op", connector.Order{ID: "PO1", AmountMinor: 5}); err != nil {
		t.Fatal(err)
	}
	if err := ds.RecordBlock(ctx, "op", "fenced"); err != nil || ds.RecordBlock(ctx, "op", "fenced") != nil {
		t.Fatal("block is idempotent")
	}
	p, err := ds.Proof(ctx, "op")
	if err != nil || !p.CreateIntent || !p.Blocked || p.CreateDone == nil || p.CreateDone.ID != "PO1" {
		t.Fatalf("proof %+v (%v)", p, err)
	}
	if err := anchor.Verify(ctx, h.Engine); err != nil {
		t.Fatal(err)
	}
}

// The write switch: turning writes off bumps the generation and blocks every
// append that requires the old one; turning them on needs the current one.
func TestAnchorWriteSwitch(t *testing.T) {
	h := dbtest.NewAnchor(t)
	ctx := context.Background()
	c := &anchor.Client{Pool: h.Engine}
	ws, err := c.WriteState(ctx)
	if err != nil || !ws.Enabled || ws.Generation != 1 {
		t.Fatalf("initial %+v (%v)", ws, err)
	}
	off, err := c.DisableWrites(ctx, "RESTORE_DETECTED")
	if err != nil || off.Enabled || off.Generation != 2 {
		t.Fatalf("off %+v (%v)", off, err)
	}
	if again, _ := c.DisableWrites(ctx, "other"); again.Generation != 2 || again.Reason != "RESTORE_DETECTED" {
		t.Fatalf("second disable %+v", again)
	}
	for _, gen := range []int64{1, 2} {
		if _, err := c.Append(ctx, anchor.DispatchIntent, "t", fmt.Sprint("op-", gen), intent("x"), gen); !errors.Is(err, anchor.ErrWritesDisabled) {
			t.Fatalf("append at generation %d while off: %v", gen, err)
		}
	}
	if _, err := c.EnableWrites(ctx, 1, "stale", 0); !errors.Is(err, anchor.ErrWritesDisabled) {
		t.Fatalf("enable with an old generation: %v", err)
	}
	on, err := c.EnableWrites(ctx, 2, "RESUME_GATE_PASSED", 7)
	if err != nil || !on.Enabled || on.Generation != 2 || on.BaselineSeq != 7 {
		t.Fatalf("on %+v (%v)", on, err)
	}
	if _, err := c.Append(ctx, anchor.DispatchIntent, "t", "op-1", intent("op-1"), 1); !errors.Is(err, anchor.ErrWritesDisabled) {
		t.Fatal("a worker from before the restore must stay refused")
	}
	if _, err := c.Append(ctx, anchor.DispatchIntent, "t", "op-1", intent("op-1"), 2); err != nil {
		t.Fatal(err)
	}
	if n, err := c.ExpectedMinSeq(ctx, "odoo"); err != nil || n != 7 {
		t.Fatalf("baseline %d (%v)", n, err)
	}
	if _, err := c.Append(ctx, anchor.Outcome, "t", "op-1", anchor.OutcomeRecord{Destination: "odoo", DestinationSeq: 12}, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.ExpectedMinSeq(ctx, "odoo"); n != 12 {
		t.Fatalf("expected min seq %d", n)
	}
}

// Concurrent appends from many writers keep one contiguous, verifiable chain,
// and nothing that requires the old generation lands after writes go off.
func TestAnchorConcurrentAppendsAndDisable(t *testing.T) {
	h := dbtest.NewAnchor(t)
	ctx := context.Background()
	c := &anchor.Client{Pool: h.Engine}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op := fmt.Sprint("op-", i)
			_, _ = c.Append(ctx, anchor.DispatchIntent, "t", op, intent(op), 1)
			if i == 20 {
				_, _ = c.DisableWrites(ctx, "RESTORE_DETECTED")
			}
		}(i)
	}
	wg.Wait()
	if err := anchor.Verify(ctx, h.Engine); err != nil {
		t.Fatal(err)
	}
	var offSeq, lateIntents int
	if err := h.Admin.QueryRow(ctx, `SELECT seq FROM anchor.engine_records WHERE record_type = 'WRITE_STATE'`).Scan(&offSeq); err != nil {
		t.Fatal(err)
	}
	if err := h.Admin.QueryRow(ctx, `SELECT count(*) FROM anchor.engine_records WHERE record_type = 'DISPATCH_INTENT' AND seq > $1`, offSeq).Scan(&lateIntents); err != nil {
		t.Fatal(err)
	}
	if lateIntents != 0 {
		t.Fatalf("%d intents were appended after writes went off", lateIntents)
	}
}
