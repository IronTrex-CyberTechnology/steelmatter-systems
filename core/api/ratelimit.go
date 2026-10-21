package api

import (
	"sync"
	"time"
)

// Limiter is a per-principal token bucket (document 03: 429 quota). Each
// authenticated principal may make Burst requests at once, refilled at
// PerMinute requests per minute. Quota is checked after authentication and
// before body parsing, so a rejected request has no effect. State is
// in-process: R0 runs one service instance.
//
// Integer nanosecond arithmetic only; no floating point.
type Limiter struct {
	PerMinute int64
	Burst     int64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens int64 // whole tokens available
	last   time.Time
}

// NewLimiter returns a limiter, or nil (no quota) when perMinute or burst is 0.
func NewLimiter(perMinute, burst int64) *Limiter {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	return &Limiter{PerMinute: perMinute, Burst: burst, now: time.Now, buckets: map[string]*bucket{}}
}

// interval is the time to earn one token.
func (l *Limiter) interval() time.Duration {
	return time.Minute / time.Duration(l.PerMinute)
}

// Allow takes one token for principal. When none is left it returns false and
// the wait until the next token.
func (l *Limiter) Allow(principal string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[principal]
	if !ok {
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[principal] = b
	}
	step := l.interval()
	if step <= 0 {
		step = 1
	}
	if earned := int64(now.Sub(b.last) / step); earned > 0 {
		b.tokens += earned
		b.last = b.last.Add(time.Duration(earned) * step)
		if b.tokens >= l.Burst {
			b.tokens, b.last = l.Burst, now
		}
	}
	if b.tokens > 0 {
		b.tokens--
		return true, 0
	}
	return false, step - now.Sub(b.last)
}
