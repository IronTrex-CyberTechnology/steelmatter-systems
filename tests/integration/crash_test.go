//go:build integration && failpoints

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds cmd/steelmatter with failpoints compiled in, once per run.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "steelmatter-crash-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "steelmatter")
		out, err := exec.Command("go", "build", "-tags", "failpoints", "-o", binPath, "../../cmd/steelmatter").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

type proc struct {
	cmd  *exec.Cmd
	url  string
	out  *bytes.Buffer
	done chan error
}

// start runs the real service against the test database with the given
// STEELMATTER_FAILPOINTS value and points e.url at it.
func (e *env) start(failpoints string) *proc {
	e.t.Helper()
	interval := "100"
	if failpoints != "" {
		// Workers run once at startup, then not again before the crash, so a
		// crash test sees exactly the state at its failpoint.
		interval = "600000"
	}
	return e.startWith(failpoints, interval)
}

func (e *env) startWith(failpoints, interval string) *proc {
	e.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	p := &proc{url: "http://" + addr, out: &bytes.Buffer{}, done: make(chan error, 1)}
	p.cmd = exec.Command(binary(e.t), "serve")
	p.cmd.Env = append(os.Environ(),
		"STEELMATTER_DATABASE_URL="+e.h.DB.ExecutorURL, "STEELMATTER_OBSERVER_URL="+e.h.DB.ObserverURL, "STEELMATTER_HTTP_ADDR="+addr, "STEELMATTER_KEYS_DIR="+e.keysDir,
		"STEELMATTER_WORKER_INTERVAL_MS="+interval, "STEELMATTER_RECOVERY_AGE_SECONDS=0", "STEELMATTER_FAILPOINTS="+failpoints)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	e.t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done: // exited during startup (e.g. a worker failpoint); let crashed() inspect it
			p.done <- err
			return p
		default:
		}
		if res, err := http.Get(p.url + "/v1/health"); err == nil {
			res.Body.Close()
			e.url = p.url
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("service did not start:\n%s", p.out)
	return nil
}

// crashed waits for the process to exit with the failpoint's 137 status.
func (p *proc) crashed(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 137 {
			t.Fatalf("process ended with %v; want exit 137\n%s", err, p.out)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("process did not crash\n%s", p.out)
	}
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

// T14: process killed after the target insert, before commit.
func TestT14PrecommitCrash(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	proc := e.start("admission.after_target_insert=exit")
	if st, _ := e.admit("agent-a", p.action, p.jws); st != 0 {
		t.Fatalf("client got a response (%d) from a crashed process", st)
	}
	proc.crashed(t)
	requireLedger(t, e.ledger(p.mandate), reservedLedger) // rolled back: no order, no consumption
	if s, _ := e.issuanceStatus(p.permitID); s != "ACTIVE" {
		t.Fatalf("permit %s after rollback", s)
	}
	e.start("")
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry after restart", st, body, 201, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger) // exactly one
}

// T15: process killed after commit, before outbox delivery.
func TestT15PostcommitCrash(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	proc := e.start("admission.after_commit=exit")
	if st, _ := e.admit("agent-a", p.action, p.jws); st != 0 {
		t.Fatalf("client got a response (%d)", st)
	}
	proc.crashed(t)
	requireLedger(t, e.ledger(p.mandate), committedLedger)
	var eventID string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT event_id FROM steelmatter.outbox WHERE delivered_at IS NULL`).Scan(&eventID); err != nil {
		t.Fatalf("expected one undelivered event: %v", err)
	}
	if e.count(`SELECT count(*) FROM steelmatter.event_receipts`) != 0 {
		t.Fatal("event delivered before the crash")
	}
	e.start("")
	e.eventually("outbox delivery after restart", func() bool {
		return e.count(`SELECT count(*) FROM steelmatter.event_receipts WHERE event_id = $1`, eventID) == 1
	})
	st, body := e.admit("agent-a", p.action, p.jws)
	requireStatus(t, "retry after restart", st, body, 200, "")
	requireLedger(t, e.ledger(p.mandate), committedLedger)
	if e.count(`SELECT count(*) FROM steelmatter.event_receipts`) != 1 {
		t.Fatal("restart published a different event ID")
	}
}

// T15 redelivery: killed after delivery but before marking; the same event ID
// is delivered again and the consumer deduplicates.
func TestT15OutboxRedeliverySameEventID(t *testing.T) {
	e := newEnv(t)
	p := e.permitted()
	st, body := e.admit("agent-a", p.action, p.jws) // in-process; no workers running
	requireStatus(t, "admit", st, body, 201, "")
	proc := e.start("outbox.after_deliver=exit")
	proc.crashed(t)
	var eventID string
	_ = e.h.Admin.QueryRow(context.Background(), `SELECT event_id FROM steelmatter.outbox`).Scan(&eventID)
	e.start("")
	e.eventually("redelivery", func() bool {
		return e.count(`SELECT count(*) FROM steelmatter.event_receipts WHERE event_id = $1 AND deliveries = 2`, eventID) == 1
	})
	e.eventually("marked delivered", func() bool {
		return e.count(`SELECT count(*) FROM steelmatter.outbox WHERE delivered_at IS NOT NULL`) == 1
	})
	requireLedger(t, e.ledger(p.mandate), committedLedger)
}

// T16: process killed after reservation, before signing. Recovery signs the
// same issuance; it is never reserved twice.
func TestT16SigningCrash(t *testing.T) {
	e := newEnv(t)
	m := e.mandate(mandateOpts{})
	e.assign(m, "agent-a")
	id := e.instruction("po-request-001", m)["instruction_id"].(string)
	proc := e.start("evaluate.after_reserve=exit")
	if st, _ := e.evaluate("agent-a", id, 1); st != 0 {
		t.Fatalf("client got a response (%d)", st)
	}
	proc.crashed(t)
	var permitID string
	if err := e.h.Admin.QueryRow(context.Background(), `SELECT permit_id FROM steelmatter.issuances WHERE status = 'UNSIGNED'`).Scan(&permitID); err != nil {
		t.Fatal(err)
	}
	requireLedger(t, e.ledger(m), reservedLedger)

	e.start("")
	e.eventually("recovery worker signs the issuance", func() bool {
		return e.count(`SELECT count(*) FROM steelmatter.issuances WHERE permit_id = $1 AND status = 'ACTIVE'`, permitID) == 1
	})
	st, body := e.evaluate("agent-a", id, 1)
	requireStatus(t, "evaluate after recovery", st, body, 200, "")
	if body["permit"].(map[string]any)["permit_id"] != permitID {
		t.Fatal("a different issuance was created")
	}
	requireLedger(t, e.ledger(m), reservedLedger)
	if e.count(`SELECT count(*) FROM steelmatter.issuances`) != 1 {
		t.Fatal("reserved twice")
	}
}
