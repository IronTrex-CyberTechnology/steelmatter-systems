package api

import (
	"testing"
	"time"
)

func TestLimiterBurstThenRefillPerPrincipal(t *testing.T) {
	l := NewLimiter(60, 3) // one token per second, burst 3
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("agent-a"); !ok {
			t.Fatalf("request %d within burst denied", i+1)
		}
	}
	ok, wait := l.Allow("agent-a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("4th request: ok=%v wait=%s", ok, wait)
	}
	if ok, _ := l.Allow("agent-b"); !ok {
		t.Fatal("another principal shares the bucket")
	}
	now = now.Add(999 * time.Millisecond)
	if ok, _ := l.Allow("agent-a"); ok {
		t.Fatal("token earned early")
	}
	now = now.Add(time.Millisecond)
	if ok, _ := l.Allow("agent-a"); !ok {
		t.Fatal("token not earned after one interval")
	}
	now = now.Add(time.Hour) // refill never exceeds the burst
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("agent-a"); !ok {
			t.Fatalf("refilled request %d denied", i+1)
		}
	}
	if ok, _ := l.Allow("agent-a"); ok {
		t.Fatal("refill exceeded burst")
	}
}

func TestNewLimiterZeroDisables(t *testing.T) {
	if NewLimiter(0, 10) != nil || NewLimiter(10, 0) != nil {
		t.Fatal("zero configuration must disable the quota")
	}
}
