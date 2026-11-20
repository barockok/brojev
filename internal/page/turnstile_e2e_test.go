package page

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/turnstile"
)

// waitWidget polls until Cloudflare's script has rendered a widget. The
// fixture needs network access; skip the test if the widget never loads.
func waitWidget(t *testing.T, p *Page, ctx context.Context) *turnstile.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := p.DetectTurnstile(ctx)
		// The widget root appears before Cloudflare injects its iframe; wait
		// for the visible widget (or a token) so mode is meaningful.
		if st.Detected && (st.Mode == turnstile.ModeManaged || st.State == turnstile.StateSolved) {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skip("Turnstile widget did not load (challenges.cloudflare.com unreachable or blocked)")
	return nil
}

func TestTurnstileDetect(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	st := waitWidget(t, p, ctx)
	if st.Sitekey != "2x00000000000000000000AB" {
		t.Fatalf("sitekey = %q", st.Sitekey)
	}
	if st.Mode != turnstile.ModeManaged {
		t.Fatalf("mode = %q", st.Mode)
	}
	if st.State != turnstile.StateUnresolved {
		t.Fatalf("state = %q, reason %q", st.State, st.Reason)
	}
	if st.Line() == "" {
		t.Fatal("missing status line")
	}

	// A page with no widget must report absent and cost one eval.
	p2, ctx2 := open(t, "/form")
	if st := p2.DetectTurnstile(ctx2); st.Detected || st.State != turnstile.StateAbsent {
		t.Fatalf("form page: %+v", st)
	}
}

type fakeSolver struct {
	token string
	got   turnstile.Target
}

func (f *fakeSolver) Solve(ctx context.Context, t turnstile.Target) (string, error) {
	f.got = t
	return f.token, nil
}

func TestTurnstileAutoPass(t *testing.T) {
	p, ctx := open(t, "/turnstile-pass")
	waitWidget(t, p, ctx)
	st := p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("pass key: %+v", st)
	}
	if st.SolverUsed {
		t.Fatal("solver used for a passing widget")
	}
}

func TestTurnstileClick(t *testing.T) {
	p, ctx := open(t, "/turnstile-fake")
	waitWidget(t, p, ctx)
	st := p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("local widget: %+v", st)
	}
	v, err := p.Eval(ctx, `document.querySelector('[name="cf-turnstile-response"]')?.value`)
	if err != nil || string(v) != `"local-token"` {
		t.Fatalf("token = %s, %v", v, err)
	}
}

func TestTurnstileForceClick(t *testing.T) {
	p, ctx := open(t, "/turnstile-force")
	waitWidget(t, p, ctx)
	st := p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("3x key: %+v", st)
	}
}

func TestTurnstileVisibleRootWithoutFrame(t *testing.T) {
	p, ctx := open(t, "/turnstile-noframe")
	st := p.DetectTurnstile(ctx)
	if !st.Detected || st.Mode != turnstile.ModeManaged {
		t.Fatalf("detection: %+v", st)
	}
	st = p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("no-frame widget: %+v", st)
	}
	v, err := p.Eval(ctx, `document.querySelector('[name="cf-turnstile-response"]')?.value`)
	if err != nil || string(v) != `"noframe-token"` {
		t.Fatalf("token = %s, %v", v, err)
	}
}

func TestTurnstileSolverInjection(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	fs := &fakeSolver{token: "test-token"}
	st := p.ensureTurnstile(ctx, turnstileOptions{
		Total: 5 * time.Second, InPage: 300 * time.Millisecond, Solver: fs,
	})
	if st == nil || st.State != turnstile.StateSolved || !st.SolverUsed {
		t.Fatalf("solver: %+v", st)
	}
	if fs.got.Sitekey != "2x00000000000000000000AB" || fs.got.URL == "" {
		t.Fatalf("solver target: %+v", fs.got)
	}
	v, err := p.Eval(ctx, `document.querySelector('[name="cf-turnstile-response"]').value`)
	if err != nil || string(v) != `"test-token"` {
		t.Fatalf("injected token = %s, %v", v, err)
	}
}

func TestTurnstileNoSolverIsReported(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	st := p.ensureTurnstile(ctx, turnstileOptions{Total: 2 * time.Second, InPage: 200 * time.Millisecond})
	if st == nil || st.State != turnstile.StateUnresolved || st.SolverUsed {
		t.Fatalf("no solver: %+v", st)
	}
	if !strings.Contains(st.Reason, "CapSolver") {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestTurnstileSnapshotLine(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	st := p.DetectTurnstile(ctx)
	s := snap(t, p, ctx)
	s.Turnstile = st
	if !strings.Contains(s.String(), "TURNSTILE: unresolved") {
		t.Fatalf("snapshot:\n%s", s)
	}
}
