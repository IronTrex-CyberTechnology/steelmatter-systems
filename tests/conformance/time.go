//go:build conformance

package conformance

import "time"

func nowUTC() time.Time { return time.Now().UTC() }
