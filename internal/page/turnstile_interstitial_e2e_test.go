package page

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/turnstile"
)

func waitTitle(t *testing.T, p *Page, ctx context.Context, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := p.Eval(ctx, `document.title`); err == nil && string(v) == `"`+want+`"` {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	v, _ := p.Eval(ctx, `document.title`)
	t.Fatalf("title = %s, want %q", v, want)
}

func TestInterstitialDetect(t *testing.T) {
	p, ctx := open(t, "/cf-challenge-blank")
	st := p.DetectTurnstile(ctx)
	if !st.Detected || st.State != turnstile.StateInterstitial || st.Mode != turnstile.ModeInterstitial {
		t.Fatalf("detection: %+v", st)
	}
	// Widget markers on a challenge page do not make it a widget.
	p2, ctx2 := open(t, "/cf-challenge-marked")
	if st := p2.DetectTurnstile(ctx2); st.State != turnstile.StateInterstitial {
		t.Fatalf("marked challenge: %+v", st)
	}
}

func TestInterstitialAutoPass(t *testing.T) {
	p, ctx := open(t, "/cf-challenge-auto")
	st := p.ensureTurnstile(ctx, turnstileOptions{Total: 10 * time.Second, InPage: 4 * time.Second})
	if st == nil || st.State != turnstile.StateSolved || st.Mode != turnstile.ModeInterstitial {
		t.Fatalf("auto pass: %+v", st)
	}
	waitTitle(t, p, ctx, "nav two")
}

func TestInterstitialClick(t *testing.T) {
	for _, path := range []string{"/cf-challenge", "/cf-challenge-marked"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			p, ctx := open(t, path)
			st := p.ensureTurnstile(ctx, turnstileOptions{Total: 15 * time.Second, InPage: 4 * time.Second})
			if st == nil || st.State != turnstile.StateSolved || st.VisualUsed || st.SolverUsed {
				t.Fatalf("in-page click: %+v", st)
			}
			waitTitle(t, p, ctx, "nav two")
		})
	}
}

func TestInterstitialVisual(t *testing.T) {
	p, ctx := open(t, "/cf-challenge-wide")
	// Without the visual phase the left-center click misses.
	st := p.ensureTurnstile(ctx, turnstileOptions{Total: 10 * time.Second, InPage: 2 * time.Second})
	if st == nil || st.State != turnstile.StateInterstitial || !strings.Contains(st.Reason, "in-page click did not pass") {
		t.Fatalf("without visual: %+v", st)
	}
	if !strings.Contains(st.Line(), "in-page click did not pass") {
		t.Fatalf("line = %q", st.Line())
	}
	_, fr, err := p.challengeFrame(ctx)
	if err != nil || fr == nil {
		t.Fatalf("challenge frame: %v %v", fr, err)
	}
	clip, err := p.interstitialClip(ctx, fr)
	if err != nil {
		t.Fatal(err)
	}
	cb := tsRect{X: fr.X + 400, Y: fr.Y + 20, W: 24, H: 24}
	fv := &fakeVisual{box: &tsRect{X: (cb.X - clip.X) / clip.W, Y: (cb.Y - clip.Y) / clip.H, W: cb.W / clip.W, H: cb.H / clip.H}}
	time.Sleep(1100 * time.Millisecond) // per-page cooldown between attempts
	st = p.ensureTurnstile(ctx, turnstileOptions{Total: 20 * time.Second, InPage: 1 * time.Second, VisualBudget: 8 * time.Second, Visual: fv})
	if st == nil || st.State != turnstile.StateSolved || !st.VisualUsed {
		t.Fatalf("visual: %+v", st)
	}
	if len(fv.img) == 0 {
		t.Fatal("locator got no image")
	}
	waitTitle(t, p, ctx, "nav two")
}

func TestInterstitialNoFrameReported(t *testing.T) {
	p, ctx := open(t, "/cf-challenge-blank")
	st := p.ensureTurnstile(ctx, turnstileOptions{Total: 5 * time.Second, InPage: 1 * time.Second})
	if st == nil || st.State != turnstile.StateInterstitial || st.SolverUsed {
		t.Fatalf("blank challenge: %+v", st)
	}
	if !strings.Contains(st.Reason, "challenge frame not found") {
		t.Fatalf("reason = %q", st.Reason)
	}
	// A third attempt on the same host is refused (two per page).
	time.Sleep(1100 * time.Millisecond)
	_ = p.ensureTurnstile(ctx, turnstileOptions{Total: 5 * time.Second, InPage: 500 * time.Millisecond})
	time.Sleep(1100 * time.Millisecond)
	st = p.ensureTurnstile(ctx, turnstileOptions{Total: 5 * time.Second, InPage: 500 * time.Millisecond})
	if st.Reason != "gave up after 2 attempts" {
		t.Fatalf("third attempt: %+v", st)
	}
}
