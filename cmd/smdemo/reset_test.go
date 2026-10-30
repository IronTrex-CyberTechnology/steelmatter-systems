//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/IronTrex-CyberTechnology/steelmatter-systems/core/dbtest"
)

// NEW-P6-SDET-01: reset must succeed on a database migrated to the current
// schema, where R1 tables refer to the R0 rows it truncates.
func TestSmdemoResetWithR1Tables(t *testing.T) {
	h := dbtest.New(t)
	for i := 0; i < 2; i++ { // a second reset of an empty state also works
		if err := reset(context.Background(), h.Admin); err != nil {
			t.Fatalf("reset %d: %v", i+1, err)
		}
	}
}
