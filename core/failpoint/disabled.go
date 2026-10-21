//go:build !failpoints

package failpoint

import "context"

// Enabled reports whether failpoints are compiled in.
const Enabled = false

func inject(context.Context, string) error { return nil }
