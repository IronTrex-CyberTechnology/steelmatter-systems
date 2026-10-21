//go:build !failpoints

package failpoint

import (
	"context"
	"testing"
)

// Release builds (no "failpoints" tag) must compile every point to a no-op.
func TestFailpointsDisabledInReleaseBuild(t *testing.T) {
	if Enabled {
		t.Fatal("failpoints enabled without the build tag")
	}
	if err := Inject(context.Background(), AdmissionAfterCommit); err != nil {
		t.Fatal(err)
	}
}
