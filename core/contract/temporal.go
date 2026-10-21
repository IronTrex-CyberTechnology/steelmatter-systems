package contract

import "time"

// Cross-record timestamp rules for an evidence chain (ADR-003). Timestamps are
// whole UTC seconds from one clock; expiry is exclusive. The TypeScript
// verifier implements the same rules with the same check names, pinned by
// fixtures/vectors/evidence-temporal-v1.json.

// TemporalStatus is the outcome of one rule.
type TemporalStatus string

const (
	TemporalOK       TemporalStatus = "OK"
	TemporalMismatch TemporalStatus = "MISMATCH"
	TemporalMissing  TemporalStatus = "MISSING" // a record the rule needs is absent
)

// TemporalCheck is one named rule and its outcome.
type TemporalCheck struct {
	Name   string
	Status TemporalStatus
}

// TemporalLinkage evaluates every rule, in a fixed order. Nil records make
// the rules that need them MISSING. A timestamp that does not parse (only
// reachable by bypassing payload validation) is a MISMATCH, never OK.
func TemporalLinkage(m *Mandate, p *Permit, e *Execution, o *Observation) []TemporalCheck {
	at := func(s string) (time.Time, bool) {
		t, err := ParseTimestamp(s)
		return t, err == nil
	}
	rule := func(name string, present bool, holds func() bool) TemporalCheck {
		switch {
		case !present:
			return TemporalCheck{name, TemporalMissing}
		case holds():
			return TemporalCheck{name, TemporalOK}
		default:
			return TemporalCheck{name, TemporalMismatch}
		}
	}
	// le reports a ≤ b; lt reports a < b; both false if either does not parse.
	le := func(a, b string) bool {
		x, ok1 := at(a)
		y, ok2 := at(b)
		return ok1 && ok2 && !y.Before(x)
	}
	lt := func(a, b string) bool {
		x, ok1 := at(a)
		y, ok2 := at(b)
		return ok1 && ok2 && x.Before(y)
	}
	return []TemporalCheck{
		rule("time:permit_issued_within_mandate", m != nil && p != nil, func() bool {
			return le(m.IssuedAt, p.IssuedAt) && lt(p.IssuedAt, m.ExpiresAt)
		}),
		rule("time:permit_expiry_within_mandate", m != nil && p != nil, func() bool { return le(p.ExpiresAt, m.ExpiresAt) }),
		rule("time:execution_within_permit", p != nil && e != nil, func() bool {
			return le(p.IssuedAt, e.CommittedAt) && lt(e.CommittedAt, p.ExpiresAt)
		}),
		rule("time:execution_within_mandate", m != nil && e != nil, func() bool {
			return le(m.IssuedAt, e.CommittedAt) && lt(e.CommittedAt, m.ExpiresAt)
		}),
		rule("time:observation_after_execution", e != nil && o != nil, func() bool { return le(e.CommittedAt, o.ObservedAt) }),
	}
}

// TemporalConsistent reports whether no rule is violated (MISSING is not a violation).
func TemporalConsistent(cs []TemporalCheck) bool {
	for _, c := range cs {
		if c.Status == TemporalMismatch {
			return false
		}
	}
	return true
}
