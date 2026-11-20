package page

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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

type fakeVisual struct {
	box *tsRect
	img []byte
}

func (f *fakeVisual) Locate(ctx context.Context, png []byte) (*turnstileVisualResult, error) {
	f.img = png
	return &turnstileVisualResult{
		Checkbox: f.box, Present: true, BoxConfidence: 0.9, State: "checkbox",
		CostUSD: 0.0001, MS: 12,
	}, nil
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

func TestTurnstileVisualPhase(t *testing.T) {
	p, ctx := open(t, "/turnstile-visual")
	st := p.DetectTurnstile(ctx)
	if !st.Detected || st.Mode != turnstile.ModeManaged {
		t.Fatalf("detection: %+v", st)
	}
	// The deterministic left-center click misses the checkbox at x=400.
	clip, err := p.turnstileVisualClip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Eval(ctx, `(() => { const r=document.getElementById('cb').getBoundingClientRect(); return {x:r.x,y:r.y,w:r.width,h:r.height}; })()`)
	if err != nil {
		t.Fatal(err)
	}
	var cb struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if err := json.Unmarshal([]byte(v), &cb); err != nil {
		t.Fatal(err)
	}
	fv := &fakeVisual{box: &tsRect{
		X: (cb.X - clip.X) / clip.W, Y: (cb.Y - clip.Y) / clip.H,
		W: cb.W / clip.W, H: cb.H / clip.H,
	}}
	st = p.ensureTurnstile(ctx, turnstileOptions{
		Total: 30 * time.Second, InPage: 200 * time.Millisecond, VisualBudget: 10 * time.Second, Visual: fv,
	})
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("visual phase: %+v", st)
	}
	if !st.VisualUsed || st.SolverUsed {
		t.Fatalf("usage: %+v", st)
	}
	if !bytes.HasPrefix(fv.img, []byte("\x89PNG")) {
		t.Fatalf("locator did not receive a PNG (%d bytes)", len(fv.img))
	}
	got, err := p.Eval(ctx, `document.querySelector('[name="cf-turnstile-response"]').value`)
	if err != nil || string(got) != `"visual-token"` {
		t.Fatalf("token = %s, %v", got, err)
	}
}

func TestTurnstileVisualJev(t *testing.T) {
	if os.Getenv("BROJEV_VISUAL_E2E") != "1" {
		t.Skip("set BROJEV_VISUAL_E2E=1 to call Jev with an image")
	}
	t.Setenv("TURNSTILE_VISUAL", "jev")
	vis, note := turnstileVisualFromEnv()
	if vis == nil {
		t.Skipf("visual locator unavailable: %s", note)
	}
	p, ctx := open(t, "/turnstile-visual")
	waitWidget(t, p, ctx)
	st := p.ensureTurnstile(ctx, turnstileOptions{
		Total: 30 * time.Second, InPage: 200 * time.Millisecond, VisualBudget: 15 * time.Second, Visual: vis,
	})
	t.Logf("status: %+v", st)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("visual Jev: %+v", st)
	}
	if !st.VisualUsed {
		t.Fatal("visual locator was not used")
	}
}
