package turnstile

import (
	"context"
	"os"
	"strings"
)

// Target is what a solver needs to produce a token.
type Target struct {
	URL     string
	Sitekey string
	Action  string
	CData   string
}

// Solver turns a Turnstile sitekey into a response token.
type Solver interface {
	Solve(ctx context.Context, t Target) (string, error)
}

// FromEnv builds the configured solver, or nil when CAPSOLVER_API_KEY is not
// set. The key comes from the plugin settings, the environment, or
// ~/.brojev/env, in that order, via the daemon key plumbing.
func FromEnv() Solver {
	key := strings.TrimSpace(os.Getenv("CAPSOLVER_API_KEY"))
	if key == "" || strings.HasPrefix(key, "${") {
		return nil
	}
	return NewCapSolver(key)
}
