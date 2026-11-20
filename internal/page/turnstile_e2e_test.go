package page

import (
	"context"
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
