package odoo

import (
	"context"
	"encoding/json"
	"time"
)

// Coverage postures (build guide step 5).
const (
	PostureEnforced    = "enforced"    // orders go through Steelmatter
	PostureObserved    = "observed"    // not prevented, but recorded and reported
	PostureUnprotected = "unprotected" // a person, key or process can bypass Steelmatter
	PostureUnknown     = "unknown"     // cannot be verified from the destination
)

// CoverageRoute is one route to a purchase order and its posture.
type CoverageRoute struct {
	ID       string          `json:"id"`
	Route    string          `json:"route"`
	Posture  string          `json:"posture"`
	Detail   string          `json:"detail"`
	Count    *int            `json:"count,omitempty"`
	Subjects json.RawMessage `json:"subjects,omitempty"` // users, keys, modules or action names, as the module reports them
}

// Coverage is the destination's coverage posture input for the engine's
// coverage view. It is evidence about routes, never a guarantee: Complete is
// always false, and Verified is false when the destination could not be
// inspected at all (module missing, call failed), in which case every route
// is unknown.
type Coverage struct {
	Verified    bool      `json:"verified"`
	Reason      string    `json:"reason,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	Database    string    `json:"database,omitempty"`
	Seq         int64     `json:"seq"`
	Incarnation string    `json:"incarnation,omitempty"`
	Module      struct {
		Name            string `json:"name"`
		Installed       bool   `json:"installed"`
		Version         string `json:"version"`
		ExpectedVersion string `json:"expected_version"`
		Odoo            string `json:"odoo"`
		Supported       bool   `json:"supported"`
	} `json:"module"`
	Anchor struct {
		Configured bool `json:"configured"`
		Reachable  bool `json:"reachable"`
	} `json:"anchor"`
	Identities          json.RawMessage `json:"identities,omitempty"`
	Routes              []CoverageRoute `json:"routes"`
	Summary             map[string]int  `json:"summary"`
	Complete            bool            `json:"complete"`
	Source              string          `json:"source"`
	ConnectorReceivedAt time.Time       `json:"connector_received_at"`
}

// Coverage reads the destination's coverage posture (sm_coverage) with the
// read-only observer identity. It never fails: if the destination cannot be
// inspected, the result is unverified and every route is unknown.
func (c *Connector) Coverage(ctx context.Context) Coverage {
	var cov Coverage
	err := c.call(ctx, observer, "steelmatter.api", "sm_coverage", nil, nil, &cov)
	now := time.Now().UTC()
	if err != nil {
		u := Coverage{Reason: reasonOf(err), GeneratedAt: now, ConnectorReceivedAt: now, Source: c.instance + "/observer",
			Routes: []CoverageRoute{{ID: "steelmatter_module", Route: "Steelmatter module and every route in this Odoo database",
				Posture: PostureUnknown, Detail: "the destination could not be inspected (" + reasonOf(err) + "): the module may be missing, " +
					"outdated or unreachable"}}}
		u.Summary = map[string]int{PostureUnknown: 1}
		return u
	}
	cov.Verified, cov.Complete = true, false
	cov.Source, cov.ConnectorReceivedAt = c.instance+"/observer", now
	cov.Summary = map[string]int{}
	for i := range cov.Routes {
		switch cov.Routes[i].Posture {
		case PostureEnforced, PostureObserved, PostureUnprotected, PostureUnknown:
		default:
			cov.Routes[i].Posture = PostureUnknown // a value we do not understand is never read as protection
		}
		cov.Summary[cov.Routes[i].Posture]++
	}
	return cov
}
