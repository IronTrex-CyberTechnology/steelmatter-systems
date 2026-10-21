//go:build failpoints

package failpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Enabled reports whether failpoints are compiled in.
const Enabled = true

// ErrInjected is returned by an "error" action.
var ErrInjected = errors.New("failpoint: injected failure")

var (
	mu      sync.Mutex
	actions = map[string]func(context.Context) error{}
)

func init() {
	// STEELMATTER_FAILPOINTS="name=exit,other=error" for subprocess crash tests.
	for _, spec := range strings.Split(os.Getenv("STEELMATTER_FAILPOINTS"), ",") {
		name, action, ok := strings.Cut(strings.TrimSpace(spec), "=")
		if !ok {
			continue
		}
		switch action {
		case "exit":
			Set(name, func(context.Context) error {
				fmt.Fprintf(os.Stderr, "failpoint %s: exiting\n", name)
				os.Exit(137) // same status as SIGKILL
				return nil
			})
		case "error":
			Set(name, func(context.Context) error { return ErrInjected })
		}
	}
}

// Set registers an action for name (tests only).
func Set(name string, fn func(context.Context) error) {
	mu.Lock()
	defer mu.Unlock()
	actions[name] = fn
}

// Clear removes every registered action.
func Clear() {
	mu.Lock()
	defer mu.Unlock()
	actions = map[string]func(context.Context) error{}
}

func inject(ctx context.Context, name string) error {
	mu.Lock()
	fn := actions[name]
	mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}
