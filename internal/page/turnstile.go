package page

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	cdppage "github.com/chromedp/cdproto/page"

	"github.com/barockok/brojev/internal/jev"
	"github.com/barockok/brojev/internal/turnstile"
)

const turnstileDetectJS = `(() => {
  const out = {present:false, interstitial:false, sitekey:"", action:"", cdata:"", token:"", root:null, url:location.href};
  const roots = [];
  const push = (el) => { if (el && !roots.includes(el)) roots.push(el); };
  for (const el of document.querySelectorAll('.cf-turnstile')) push(el);
  for (const el of document.querySelectorAll('[data-sitekey]')) {
    const k = (el.getAttribute('data-sitekey') || '').trim();
    if (/^[0-3]x[0-9A-Za-z]/.test(k)) push(el);
  }
  const tokEl = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
  if (!roots.length && tokEl) push(tokEl.closest('.cf-turnstile') || tokEl.parentElement || tokEl);
  if (!roots.length) {
    const title = (document.title || "").trim().toLowerCase();
    out.interstitial = !!document.querySelector('#challenge-stage, #cf-challenge-running, .cf-browser-verification')
      || title === "just a moment..." || title.indexOf("attention required") === 0;
    return out;
  }
  out.present = true;
  const root = roots[0];
  const get = (k) => (root.getAttribute && root.getAttribute(k)) || (root.querySelector && root.querySelector("[" + k + "]")?.getAttribute(k)) || "";
  out.sitekey = get("data-sitekey");
  out.action = get("data-action");
  out.cdata = get("data-cdata");
  const tok = (root.querySelector && root.querySelector('[name="cf-turnstile-response"]')) || tokEl;
  out.token = tok ? String(tok.value || "") : "";
  const r = root.getBoundingClientRect();
  out.root = {x:r.x, y:r.y, w:r.width, h:r.height};
  return out;
})()`

const turnstileWidgetJS = `(() => {
  const box = (el) => { const r = el.getBoundingClientRect(); return {x:r.x, y:r.y, w:r.width, h:r.height}; };
  const shown = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const deep = (sel) => {
    const out = [];
    const visit = (r) => {
      out.push(...r.querySelectorAll(sel));
      for (const el of r.querySelectorAll('*')) if (el.shadowRoot) visit(el.shadowRoot);
    };
    visit(document);
    return out;
  };
  let cb = deep('input[type="checkbox"], [role="checkbox"]').find(shown);
  const text = ((document.body && document.body.innerText) || "").replace(/\s+/g, " ").trim().slice(0, 300);
  return {checkbox: cb ? box(cb) : null, text};
})()`

type tsRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type tsWidget struct {
	Present      bool    `json:"present"`
	Interstitial bool    `json:"interstitial"`
	Sitekey      string  `json:"sitekey"`
	Action       string  `json:"action"`
	CData        string  `json:"cdata"`
	Token        string  `json:"token"`
	Mode         string  `json:"-"`
	Widget       *tsRect `json:"-"`
	Root         *tsRect `json:"root"`
	URL          string  `json:"url"`
}

type tsInner struct {
	Checkbox *tsRect `json:"checkbox"`
	Text     string  `json:"text"`
}

// detectTurnstile returns the widget model, its frame (when brojev has a
// session for it), and the widget-internal state. The widget iframe can sit
// in a closed shadow root, so the frame is found through the CDP frame tree
// and its box, not through document.querySelector.
func (p *Page) detectTurnstile(ctx context.Context) (*tsWidget, *frame, *tsInner, error) {
	if p.OpenDialog() != nil {
		return nil, nil, nil, ErrDialogOpen
	}
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var w tsWidget
	if err := p.eval(ctx, fs[0], turnstileDetectJS, &w); err != nil {
		return nil, nil, nil, err
	}
	if !w.Present {
		return &w, nil, nil, nil
	}
	var wf *frame
	rootManaged := w.Root != nil && w.Root.W >= 100 && w.Root.H >= 30
	if len(fs) > 1 {
		origin, clip := p.offsets(ctx, fs)
		wf = pickTurnstileFrame(fs, origin, clip, w.Root)
		if wf != nil {
			o, ok := origin[wf.ID]
			if ok {
				w.Widget = &tsRect{X: o.X, Y: o.Y, W: o.W, H: o.H}
			}
			c := clip[wf.ID]
			switch {
			case c.W > 0 && c.H > 0:
				w.Mode = "managed"
			case rootManaged:
				// Explicit-render widgets can draw the checkbox in a closed
				// shadow root with no frame of their own. A visible rendered
				// root is enough to treat it as managed.
				w.Mode = "managed"
				w.Widget = w.Root
			default:
				w.Mode = "invisible"
			}
		}
	}
	if w.Mode == "" {
		if rootManaged {
			w.Mode = "managed"
		} else {
			w.Mode = "invisible"
		}
	}
	if w.Widget == nil {
		w.Widget = w.Root
	}
	var inner *tsInner
	if wf != nil {
		var got tsInner
		if p.eval(ctx, wf, turnstileWidgetJS, &got) == nil {
			inner = &got
		}
	}
	return &w, wf, inner, nil
}

// pickTurnstileFrame prefers the widget's own Cloudflare Turnstile frame,
// then any child frame drawn inside the widget root (this also covers local
// test widgets), then any other Cloudflare challenge frame.
func pickTurnstileFrame(fs []*frame, origin, clip map[cdproto.FrameID]box, root *tsRect) *frame {
	var cf, contained *frame
	for _, f := range fs[1:] {
		o, ok := origin[f.ID]
		if !ok {
			continue
		}
		if strings.Contains(f.URL, "challenges.cloudflare.com") {
			if strings.Contains(f.URL, "/turnstile/") {
				return f
			}
			if cf == nil {
				cf = f
			}
		}
		if root != nil && rectContains(root, o) {
			c := clip[f.ID]
			if contained == nil || (c.W > 0 && c.H > 0) {
				contained = f
			}
		}
	}
	if contained != nil {
		return contained
	}
	return cf
}

func rectContains(root *tsRect, o box) bool {
	const pad = 8
	return root.W > 0 && root.H > 0 &&
		o.X+o.W > root.X-pad && o.X < root.X+root.W+pad &&
		o.Y+o.H > root.Y-pad && o.Y < root.Y+root.H+pad
}

// DetectTurnstile reads the page and reports the widget without acting.
func (p *Page) DetectTurnstile(ctx context.Context) *turnstile.Status {
	w, _, inner, err := p.detectTurnstile(ctx)
	if err != nil {
		return &turnstile.Status{State: turnstile.StateUnresolved, Reason: "detection failed: " + err.Error()}
	}
	return statusOf(w, inner)
}

func statusOf(w *tsWidget, inner *tsInner) *turnstile.Status {
	st := &turnstile.Status{State: turnstile.StateAbsent}
	if w == nil {
		return st
	}
	if w.Interstitial {
		st.Detected = true
		st.State = turnstile.StateInterstitial
		st.Reason = "full-page Cloudflare challenge"
		return st
	}
	if !w.Present {
		return st
	}
	st.Detected = true
	st.Sitekey = w.Sitekey
	st.Action = w.Action
	st.CData = w.CData
	st.Mode = w.Mode
	if w.Token != "" {
		st.State = turnstile.StateSolved
		return st
	}
	st.State = turnstile.StateUnresolved
	if inner != nil && turnstileWidgetError(inner.Text) {
		st.Reason = "widget error: " + clip(inner.Text, 120)
	} else {
		st.Reason = "waiting for token"
	}
	return st
}

func turnstileWidgetError(s string) bool {
	l := strings.ToLower(s)
	for _, k := range []string{"verification failed", "unable to verify", "invalid site", "expired", "please try again", "error code", "challenge failed"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

type tsAttempt struct {
	cycles int
	last   time.Time
}

type turnstileOptions struct {
	Total        time.Duration
	InPage       time.Duration
	VisualBudget time.Duration
	Solver       turnstile.Solver
	Visual       turnstileVisual
	VisualNote   string
}

// turnstileVisual is the opt-in Jev image locator. It returns normalized
// image coordinates so the caller can translate them with the clip.
type turnstileVisual interface {
	Locate(ctx context.Context, png []byte) (*turnstileVisualResult, error)
}

type turnstileVisualResult struct {
	Checkbox      *tsRect
	Widget        *tsRect
	Present       bool
	BoxConfidence float64
	State         string
	CostUSD       float64
	MS            int64
}

type jevVisual struct{ c *jev.Client }

func (v *jevVisual) Locate(ctx context.Context, png []byte) (*turnstileVisualResult, error) {
	const ref = "shot"
	assets := map[string]jev.Asset{
		ref: {Type: "image", Format: "png", DataBase64: base64.StdEncoding.EncodeToString(png)},
	}
	qs := map[string]jev.Question{
		"checkbox": {
			Type:         "box",
			Instructions: `the small square checkbox immediately to the left of the words "Verify you are human" in shot`,
		},
		"widget": {
			Type:         "box",
			Instructions: `the entire Cloudflare verification widget, including its checkbox, text and logo, in shot`,
		},
		"checkbox_present": {
			Type:         "noul",
			Instructions: `Is an empty square checkbox next to the words "Verify you are human" visible in shot?`,
		},
		"state": {
			Type:         "choice",
			Instructions: "What is the Cloudflare verification widget in shot showing?",
			Criteria: map[string]any{
				"checkbox":        "An empty square checkbox next to the text Verify you are human, waiting for a click.",
				"spinner":         "A spinner or loading indicator, the widget is checking.",
				"verified":        "A green checkmark, or text saying Success or Verified.",
				"image_challenge": "A grid of images, or a request to select images.",
				"other":           "Anything else, including an error or a widget that failed to load.",
			},
		},
	}
	resp, err := v.c.AskQuestions(ctx, map[string]any{"shot": ref}, assets, qs)
	if err != nil {
		return nil, err
	}
	out := &turnstileVisualResult{MS: resp.Latency.Milliseconds(), CostUSD: jev.Cost(resp.Usage)}
	if a, ok := resp.Answers["checkbox"]; ok {
		out.BoxConfidence = a.Confidence
		if a.Box != nil {
			out.Checkbox = &tsRect{X: a.Box.Left, Y: a.Box.Top, W: a.Box.Right - a.Box.Left, H: a.Box.Bottom - a.Box.Top}
		}
	}
	if a, ok := resp.Answers["widget"]; ok && a.Box != nil {
		out.Widget = &tsRect{X: a.Box.Left, Y: a.Box.Top, W: a.Box.Right - a.Box.Left, H: a.Box.Bottom - a.Box.Top}
	}
	if a, ok := resp.Answers["checkbox_present"]; ok {
		out.Present = a.Noul >= 0.5
	}
	if a, ok := resp.Answers["state"]; ok {
		out.State = a.Choice
	}
	return out, nil
}

// turnstileVisualFromEnv builds the opt-in visual locator. The second return
// value explains why it is missing when TURNSTILE_VISUAL=jev was requested.
func turnstileVisualFromEnv() (turnstileVisual, string) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("TURNSTILE_VISUAL")), "jev") {
		return nil, ""
	}
	c, err := jev.FromEnv()
	if err != nil {
		return nil, "TURNSTILE_VISUAL=jev but " + err.Error()
	}
	return &jevVisual{c: c}, ""
}

func turnstileTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("TURNSTILE_TIMEOUT_MS"))
	if v == "" {
		return 60 * time.Second
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 60 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// EnsureTurnstile detects and, when needed, resolves a Turnstile on this page.
// It is safe to call after every action: an absent widget costs one eval.
// It never returns a hard error; failures are in Status.Reason.
func (p *Page) EnsureTurnstile(ctx context.Context) *turnstile.Status {
	vis, visNote := turnstileVisualFromEnv()
	return p.ensureTurnstile(ctx, turnstileOptions{
		Total: turnstileTimeout(), InPage: 8 * time.Second, VisualBudget: 8 * time.Second,
		Solver: turnstile.FromEnv(), Visual: vis, VisualNote: visNote,
	})
}

func (p *Page) ensureTurnstile(ctx context.Context, o turnstileOptions) *turnstile.Status {
	if p.OpenDialog() != nil {
		return nil
	}
	p.tsMu.Lock()
	defer p.tsMu.Unlock()

	total := o.Total
	if total <= 0 {
		total = 60 * time.Second
	}
	inPage := o.InPage
	if inPage <= 0 {
		inPage = 8 * time.Second
	}
	if inPage > total {
		inPage = total
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	w, wf, inner, err := p.detectTurnstile(ctx)
	if err != nil {
		if errors.Is(err, ErrDialogOpen) {
			return nil
		}
		return &turnstile.Status{State: turnstile.StateUnresolved, Reason: "detection failed: " + err.Error()}
	}
	if w == nil || (!w.Present && !w.Interstitial) {
		return &turnstile.Status{State: turnstile.StateAbsent}
	}
	st := statusOf(w, inner)
	if w.Interstitial || w.Token != "" {
		return st
	}

	key := w.URL + "|" + w.Sitekey
	if p.tsTries == nil || len(p.tsTries) > 64 {
		p.tsTries = map[string]tsAttempt{}
	}
	a := p.tsTries[key]
	st.Attempts = a.cycles
	if a.cycles >= 2 {
		st.State = turnstile.StateUnresolved
		st.Reason = "gave up after 2 attempts"
		return st
	}
	if !a.last.IsZero() && time.Since(a.last) < time.Second {
		st.State = turnstile.StateUnresolved
		if st.Reason == "" {
			st.Reason = "cooling down"
		}
		return st
	}
	a.cycles++
	a.last = time.Now()
	p.tsTries[key] = a
	st.Attempts = a.cycles

	if p.tryTurnstileInPage(ctx, w, wf, inner, inPage) {
		delete(p.tsTries, key)
		fresh, _, _, derr := p.detectTurnstile(ctx)
		if derr != nil {
			return &turnstile.Status{
				Detected: true, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData,
				Mode: w.Mode, State: turnstile.StateSolved, Attempts: a.cycles,
			}
		}
		final := statusOf(fresh, nil)
		final.Attempts = a.cycles
		return final
	}

	visualReason := ""
	if o.Visual != nil {
		vb := o.VisualBudget
		if vb <= 0 {
			vb = 8 * time.Second
		}
		if vb > total {
			vb = total
		}
		ok, reason, used, cost, ms := p.tryTurnstileVisual(ctx, o.Visual, vb)
		st.VisualUsed = used
		st.VisualMS = ms
		st.CostUSD += cost
		if ok {
			delete(p.tsTries, key)
			fresh, _, _, derr := p.detectTurnstile(ctx)
			if derr != nil {
				return &turnstile.Status{
					Detected: true, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData,
					Mode: w.Mode, State: turnstile.StateSolved, Attempts: a.cycles,
					VisualUsed: true, VisualMS: ms, CostUSD: st.CostUSD,
				}
			}
			final := statusOf(fresh, nil)
			final.Attempts, final.VisualUsed, final.VisualMS, final.CostUSD = a.cycles, true, ms, st.CostUSD
			return final
		}
		visualReason = reason
	} else if o.VisualNote != "" {
		visualReason = o.VisualNote
	}

	if o.Solver == nil {
		st.State = turnstile.StateUnresolved
		if visualReason != "" {
			st.Reason = "in-page failed; " + visualReason + "; CapSolver not configured"
		} else {
			st.Reason = "in-page attempt failed; CapSolver not configured"
		}
		return st
	}
	if w.Sitekey == "" {
		st.State = turnstile.StateUnresolved
		st.Reason = "in-page attempt failed; sitekey missing"
		return st
	}
	st.SolverUsed = true
	st.CostUSD += turnstile.CostPerSolveUSD
	token, err := o.Solver.Solve(ctx, turnstile.Target{URL: w.URL, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData})
	if err != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "solver failed: " + err.Error()
		if visualReason != "" {
			st.Reason = "visual: " + visualReason + "; " + st.Reason
		}
		return st
	}
	if err := p.injectTurnstileToken(ctx, token); err != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "token injection failed: " + err.Error()
		return st
	}
	fresh, _, freshInner, derr := p.detectTurnstile(ctx)
	if derr != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "verification failed: " + derr.Error()
		return st
	}
	final := statusOf(fresh, freshInner)
	final.Attempts, final.SolverUsed, final.CostUSD = a.cycles, true, st.CostUSD
	final.VisualUsed, final.VisualMS = st.VisualUsed, st.VisualMS
	if final.State != turnstile.StateSolved {
		final.State = turnstile.StateUnresolved
		final.Reason = "solver returned a token but the page did not accept it"
		return final
	}
	delete(p.tsTries, key)
	return final
}

const turnstileTokenJS = `(() => {
  const el = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
  return el ? String(el.value || '') : '';
})()`

func (p *Page) turnstileToken(ctx context.Context) string {
	v, err := p.Eval(ctx, turnstileTokenJS)
	if err != nil {
		return ""
	}
	var s string
	if jsonv2.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func (p *Page) tryTurnstileInPage(ctx context.Context, w *tsWidget, wf *frame, inner *tsInner, budget time.Duration) bool {
	if budget <= 0 {
		budget = 8 * time.Second
	}
	_ = p.Front(ctx)
	start := time.Now()
	clickedCheckbox := false
	click := func() {
		if inner != nil && inner.Checkbox != nil && wf != nil {
			if p.clickTurnstileCheckbox(ctx, wf, inner.Checkbox) == nil {
				clickedCheckbox = true
				return
			}
		}
		if w.Widget != nil {
			p.clickTurnstileWidget(ctx, w.Widget)
		}
	}
	click()
	lastDetect := start
	retried := false
	for {
		if p.turnstileToken(ctx) != "" {
			return true
		}
		if time.Since(start) > budget {
			final, fcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			ok := p.turnstileToken(final) != ""
			fcancel()
			return ok
		}
		// The script renders the widget iframe after the root appears; pick
		// up a checkbox that shows up during the wait.
		if time.Since(lastDetect) > 750*time.Millisecond {
			lastDetect = time.Now()
			if cw, cwf, cinner, err := p.detectTurnstile(ctx); err == nil {
				if cw.Widget != nil {
					w.Widget = cw.Widget
				}
				wf, inner = cwf, cinner
				if inner != nil && inner.Checkbox != nil && !clickedCheckbox {
					click()
				}
			}
		}
		if !retried && time.Since(start) > 3*time.Second {
			retried = true
			if inner != nil && turnstileWidgetError(inner.Text) {
				p.resetTurnstileWidget(ctx)
			}
			click()
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (p *Page) tryTurnstileVisual(ctx context.Context, v turnstileVisual, budget time.Duration) (bool, string, bool, float64, int64) {
	if budget <= 0 {
		budget = 8 * time.Second
	}
	// A background tab can stall Page.captureScreenshot on headless Linux
	// even when focus emulation reports it visible.
	_, _ = cdproto.Call(ctx, p.S, cdppage.BringToFront, cdproto.Empty{})
	start := time.Now()
	clip, err := p.turnstileVisualClip(ctx)
	if err != nil {
		return false, "clip failed: " + err.Error(), false, 0, 0
	}
	img, err := p.ScreenshotClip(ctx, clip)
	if err != nil {
		return false, "screenshot failed: " + err.Error(), false, 0, 0
	}
	// The HTTP call may be canceled safely; CDP calls above use the long
	// context so a phase deadline cannot poison the browser connection.
	lctx, lcancel := context.WithTimeout(ctx, budget)
	res, err := v.Locate(lctx, img)
	lcancel()
	if err != nil {
		return false, "jev failed: " + err.Error(), true, 0, 0
	}
	if res == nil {
		return false, "jev returned nothing", true, 0, 0
	}
	cost, ms := res.CostUSD, res.MS
	switch res.State {
	case "verified":
		if p.turnstileToken(ctx) != "" {
			return true, "", true, cost, ms
		}
		return false, "verified but no token", true, cost, ms
	case "image_challenge":
		return false, "image challenge", true, cost, ms
	}
	if !res.Present || res.Checkbox == nil || res.BoxConfidence < 0.5 {
		return false, fmt.Sprintf("checkbox not located (present=%v conf=%.2f)", res.Present, res.BoxConfidence), true, cost, ms
	}
	box := res.Checkbox
	cx := clip.X + (box.X+box.W/2)*clip.W
	cy := clip.Y + (box.Y+box.H/2)*clip.H
	if _, err := p.ClickXY(ctx, cx, cy, ClickOptions{}); err != nil {
		return false, "click failed: " + err.Error(), true, cost, ms
	}
	start = time.Now()
	retried := false
	for {
		if p.turnstileToken(ctx) != "" {
			return true, "", true, cost, ms
		}
		if time.Since(start) > budget {
			return false, "no token after visual click", true, cost, ms
		}
		if !retried && time.Since(start) > 1500*time.Millisecond {
			retried = true
			_, _ = p.ClickXY(ctx, cx, cy, ClickOptions{})
		}
		select {
		case <-ctx.Done():
			return false, "no token after visual click", true, cost, ms
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// turnstileVisualClip frames the widget plus 40 px of context, caps the crop
// for the image API, and picks a scale inside its 256 to 1568 px window.
func (p *Page) turnstileVisualClip(ctx context.Context) (Clip, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return Clip{}, err
	}
	if len(fs) == 0 {
		return Clip{}, errors.New("no frames")
	}
	_ = p.eval(ctx, fs[0], `(() => {
	  const root = document.querySelector('.cf-turnstile') || document.querySelector('[data-sitekey]');
	  if (root) root.scrollIntoView({block:'center', inline:'center', behavior:'instant'});
	  return true;
	})()`, nil)
	w, _, _, err := p.detectTurnstile(ctx)
	if err != nil {
		return Clip{}, err
	}
	if w == nil || (!w.Present && !w.Interstitial) {
		return Clip{}, errors.New("no widget")
	}
	base := w.Widget
	if base == nil || base.W <= 0 || base.H <= 0 {
		base = w.Root
	}
	if base == nil || base.W <= 0 || base.H <= 0 {
		return Clip{}, errors.New("widget has no box")
	}
	var vp struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if err := p.eval(ctx, fs[0], "({w:innerWidth,h:innerHeight})", &vp); err != nil {
		return Clip{}, err
	}
	if vp.W <= 0 || vp.H <= 0 {
		return Clip{}, errors.New("viewport unavailable")
	}
	x0 := math.Max(0, base.X-40)
	y0 := math.Max(0, base.Y-40)
	cw := math.Min(base.W, 900) + 80
	ch := math.Min(base.H, 400) + 80
	if x1 := math.Min(vp.W, x0+cw); x1 > x0 {
		cw = x1 - x0
	} else {
		cw = vp.W - x0
	}
	if y1 := math.Min(vp.H, y0+ch); y1 > y0 {
		ch = y1 - y0
	} else {
		ch = vp.H - y0
	}
	if cw <= 0 || ch <= 0 {
		return Clip{}, errors.New("widget is outside the viewport")
	}
	if maxW := ch * (1568.0 / 256.0); cw > maxW {
		cw = maxW
	}
	scale := 2.0
	if s := 256 / math.Min(cw, ch); scale < s {
		scale = s
	}
	if s := 1568 / math.Max(cw, ch); scale > s {
		scale = s
	}
	if scale <= 0 {
		scale = 1
	}
	return Clip{X: x0, Y: y0, W: cw, H: ch, Scale: scale}, nil
}

func (p *Page) resetTurnstileWidget(ctx context.Context) {
	_, _ = p.Eval(ctx, `(() => {
	  if (window.turnstile && window.turnstile.reset) { window.turnstile.reset(); return true; }
	  return false;
	})()`)
}

func (p *Page) clickTurnstileCheckbox(ctx context.Context, wf *frame, cb *tsRect) error {
	p.revealFrameChain(ctx, wf)
	fs, err := p.frames(ctx)
	if err != nil {
		return err
	}
	origin, _ := p.offsets(ctx, fs)
	o, ok := origin[wf.ID]
	if !ok {
		return errors.New("turnstile frame is not visible")
	}
	x, y := o.X+cb.X+cb.W/2, o.Y+cb.Y+cb.H/2
	_, err = p.ClickXY(ctx, x, y, ClickOptions{})
	return err
}

func (p *Page) clickTurnstileWidget(ctx context.Context, r *tsRect) {
	fs, err := p.frames(ctx)
	if err != nil || len(fs) == 0 {
		return
	}
	_ = p.eval(ctx, fs[0], `(() => {
	  const root = document.querySelector('.cf-turnstile') || document.querySelector('[data-sitekey]');
	  if (root) root.scrollIntoView({block:'center', inline:'center', behavior:'instant'});
	  return true;
	})()`, nil)
	w, _, _, err := p.detectTurnstile(ctx)
	if err != nil || w.Widget == nil {
		return
	}
	r = w.Widget
	x := r.X + math.Min(24, r.W*0.25)
	y := r.Y + r.H/2
	_, _ = p.ClickXY(ctx, x, y, ClickOptions{})
}

func (p *Page) revealFrameChain(ctx context.Context, f *frame) {
	fs, err := p.frames(ctx)
	if err != nil {
		return
	}
	byID := map[cdproto.FrameID]*frame{}
	for _, x := range fs {
		byID[x.ID] = x
	}
	var chain []*frame
	for x := f; x != nil && x.num != 0; x = byID[x.Parent] {
		chain = append([]*frame{x}, chain...)
	}
	for _, x := range chain {
		if parent := byID[x.Parent]; parent != nil {
			_ = p.revealOwner(ctx, x, parent)
		}
	}
}

func (p *Page) injectTurnstileToken(ctx context.Context, token string) error {
	js := `(() => {
	  const token = ` + jsString(token) + `;
	  let el = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
	  if (!el) {
	    const root = document.querySelector('.cf-turnstile') || document.body;
	    el = document.createElement('input');
	    el.type = 'hidden';
	    el.name = 'cf-turnstile-response';
	    root.appendChild(el);
	  }
	  const proto = el.localName === 'textarea' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
	  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, token);
	  el.dispatchEvent(new Event('input', {bubbles:true, composed:true}));
	  el.dispatchEvent(new Event('change', {bubbles:true}));
	  return el.value;
	})()`
	v, err := p.Eval(ctx, js)
	if err != nil {
		return err
	}
	var got string
	if err := jsonv2.Unmarshal(v, &got); err != nil {
		return err
	}
	if got != token {
		return errors.New("response input did not take the token")
	}
	return nil
}
