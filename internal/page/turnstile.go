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

const turnstileRootsJS = `(() => {
  const roots = [];
  const push = (el) => { if (el && !roots.includes(el)) roots.push(el); };
  for (const el of document.querySelectorAll('.cf-turnstile')) push(el);
  for (const el of document.querySelectorAll('[data-sitekey]')) {
    const k = (el.getAttribute('data-sitekey') || '').trim();
    if (/^[0-3]x[0-9A-Za-z]/.test(k)) push(el);
  }
  for (const input of document.querySelectorAll('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]')) {
    push(input.closest('.cf-turnstile') || input.parentElement || input);
  }
  return roots.filter(el => !roots.some(o => o !== el && o.contains(el)));
})()`

const turnstileDetectJS = `(() => {
  const out = {interstitial:false, widgets:[], url:location.href};
  const top = ` + turnstileRootsJS + `;
  if (!top.length) {
    const title = (document.title || "").trim().toLowerCase();
    out.interstitial = !!document.querySelector('#challenge-stage, #cf-challenge-running, .cf-browser-verification')
      || title === "just a moment..." || title.indexOf("attention required") === 0;
    return out;
  }
  const fallback = top.length === 1 ? document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]') : null;
  for (const root of top) {
    const get = (k) => (root.getAttribute && root.getAttribute(k)) || (root.querySelector && root.querySelector("[" + k + "]")?.getAttribute(k)) || "";
    const tok = (root.querySelector && root.querySelector('[name="cf-turnstile-response"]')) || fallback;
    const r = root.getBoundingClientRect();
    out.widgets.push({
      sitekey: get("data-sitekey"),
      action: get("data-action"),
      cdata: get("data-cdata"),
      token: tok ? String(tok.value || "") : "",
      root: {x:r.x, y:r.y, w:r.width, h:r.height}
    });
  }
  return out;
})()`

func turnstileTokenAtJS(i int) string {
	return fmt.Sprintf(`(() => {
  const top = %s;
  const root = top[%d];
  const sel = 'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]';
  const el = (root && root.querySelector(sel)) || (top.length === 1 ? document.querySelector(sel) : null);
  return el ? String(el.value || "") : "";
})()`, turnstileRootsJS, i)
}

func turnstileScrollAtJS(i int) string {
	return fmt.Sprintf(`(() => {
  const top = %s;
  const root = top[%d];
  if (!root) return false;
  root.scrollIntoView({block:'center', inline:'center', behavior:'instant'});
  return true;
})()`, turnstileRootsJS, i)
}

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
	Index        int     `json:"-"`
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

type tsWidgetInfo struct {
	w     *tsWidget
	frame *frame
	inner *tsInner
}

type tsInner struct {
	Checkbox *tsRect `json:"checkbox"`
	Text     string  `json:"text"`
}

// detectTurnstiles returns every widget on the page, each with its frame
// (when brojev has a session for it) and the widget-internal state. Widget
// iframes can sit in closed shadow roots, so frames are found through the CDP
// frame tree and their boxes, not through document.querySelector.
func (p *Page) detectTurnstiles(ctx context.Context) ([]*tsWidgetInfo, bool, error) {
	if p.OpenDialog() != nil {
		return nil, false, ErrDialogOpen
	}
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, false, err
	}
	var raw struct {
		Interstitial bool   `json:"interstitial"`
		URL          string `json:"url"`
		Widgets      []struct {
			Sitekey string  `json:"sitekey"`
			Action  string  `json:"action"`
			CData   string  `json:"cdata"`
			Token   string  `json:"token"`
			Root    *tsRect `json:"root"`
		} `json:"widgets"`
	}
	if err := p.eval(ctx, fs[0], turnstileDetectJS, &raw); err != nil {
		return nil, false, err
	}
	if raw.Interstitial {
		return nil, true, nil
	}
	if len(raw.Widgets) == 0 {
		return nil, false, nil
	}
	var origin, clip map[cdproto.FrameID]box
	if len(fs) > 1 {
		origin, clip = p.offsets(ctx, fs)
	}
	used := map[cdproto.FrameID]bool{}
	out := make([]*tsWidgetInfo, 0, len(raw.Widgets))
	for i, rw := range raw.Widgets {
		w := &tsWidget{
			Index: i, Present: true, Sitekey: rw.Sitekey, Action: rw.Action,
			CData: rw.CData, Token: rw.Token, Root: rw.Root, URL: raw.URL,
		}
		var wf *frame
		if len(fs) > 1 {
			wf = pickTurnstileFrame(fs, origin, clip, rw.Root, used)
		}
		if wf != nil {
			used[wf.ID] = true
			if o, ok := origin[wf.ID]; ok {
				w.Widget = &tsRect{X: o.X, Y: o.Y, W: o.W, H: o.H}
			}
			c := clip[wf.ID]
			switch {
			case c.W > 0 && c.H > 0:
				w.Mode = "managed"
			case rootManaged(rw.Root):
				// Explicit-render widgets can draw the checkbox in a closed
				// shadow root with no frame of their own. A visible rendered
				// root is enough to treat it as managed.
				w.Mode = "managed"
				w.Widget = rw.Root
			default:
				w.Mode = "invisible"
			}
		}
		if w.Mode == "" {
			if rootManaged(rw.Root) {
				w.Mode = "managed"
			} else {
				w.Mode = "invisible"
			}
		}
		if w.Widget == nil {
			w.Widget = rw.Root
		}
		var inner *tsInner
		if wf != nil {
			var got tsInner
			if p.eval(ctx, wf, turnstileWidgetJS, &got) == nil {
				inner = &got
			}
		}
		out = append(out, &tsWidgetInfo{w: w, frame: wf, inner: inner})
	}
	return out, false, nil
}

func rootManaged(r *tsRect) bool { return r != nil && r.W >= 100 && r.H >= 30 }

// detectTurnstile keeps the single-widget shape for callers that only need
// the first widget.
func (p *Page) detectTurnstile(ctx context.Context) (*tsWidget, *frame, *tsInner, error) {
	ws, _, err := p.detectTurnstiles(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ws) == 0 {
		return &tsWidget{}, nil, nil, nil
	}
	return ws[0].w, ws[0].frame, ws[0].inner, nil
}

// pickTurnstileFrame prefers a child frame drawn inside this widget root
// (the local and Cloudflare cases), then any unclaimed Cloudflare frame.
func pickTurnstileFrame(fs []*frame, origin, clip map[cdproto.FrameID]box, root *tsRect, used map[cdproto.FrameID]bool) *frame {
	var cf, contained *frame
	for _, f := range fs[1:] {
		if used != nil && used[f.ID] {
			continue
		}
		o, ok := origin[f.ID]
		if !ok {
			continue
		}
		if root != nil && rectContains(root, o) {
			c := clip[f.ID]
			if contained == nil || (c.W > 0 && c.H > 0) {
				contained = f
			}
		}
		if strings.Contains(f.URL, "challenges.cloudflare.com") && cf == nil {
			cf = f
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

// DetectTurnstile reads the page and reports every widget without acting.
func (p *Page) DetectTurnstile(ctx context.Context) *turnstile.Status {
	ws, interstitial, err := p.detectTurnstiles(ctx)
	if err != nil {
		return &turnstile.Status{State: turnstile.StateUnresolved, Reason: "detection failed: " + err.Error()}
	}
	if interstitial {
		return &turnstile.Status{Detected: true, State: turnstile.StateInterstitial, Reason: "full-page Cloudflare challenge"}
	}
	return statusOfMany(ws)
}

func statusOfMany(ws []*tsWidgetInfo) *turnstile.Status {
	st := &turnstile.Status{State: turnstile.StateAbsent}
	if len(ws) == 0 {
		return st
	}
	st.Detected = true
	st.Widgets = len(ws)
	unsolved := 0
	var first *tsWidgetInfo
	for _, wi := range ws {
		if wi.w.Token == "" {
			unsolved++
			if first == nil {
				first = wi
			}
		}
	}
	st.Unsolved = unsolved
	if unsolved == 0 {
		st.State = turnstile.StateSolved
		first = ws[0]
	} else {
		st.State = turnstile.StateUnresolved
		switch {
		case len(ws) > 1:
			st.Reason = fmt.Sprintf("%d of %d widgets unsolved", unsolved, len(ws))
		case first != nil && first.inner != nil && turnstileWidgetError(first.inner.Text):
			st.Reason = "widget error: " + clip(first.inner.Text, 120)
		default:
			st.Reason = "waiting for token"
		}
	}
	if first != nil {
		st.Sitekey = first.w.Sitekey
		st.Action = first.w.Action
		st.CData = first.w.CData
		st.Mode = first.w.Mode
	}
	return st
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

	ws, interstitial, err := p.detectTurnstiles(ctx)
	if err != nil {
		if errors.Is(err, ErrDialogOpen) {
			return nil
		}
		return &turnstile.Status{State: turnstile.StateUnresolved, Reason: "detection failed: " + err.Error()}
	}
	if interstitial {
		return &turnstile.Status{Detected: true, State: turnstile.StateInterstitial, Reason: "full-page Cloudflare challenge"}
	}
	if len(ws) == 0 {
		return &turnstile.Status{State: turnstile.StateAbsent}
	}
	initial := statusOfMany(ws)
	if initial.State == turnstile.StateSolved {
		return initial
	}
	if p.tsTries == nil || len(p.tsTries) > 128 {
		p.tsTries = map[string]tsAttempt{}
	}

	var attempts int
	var solverUsed, visualUsed bool
	var cost float64
	var visualMS int64
	var reasons []string
	for i, wi := range ws {
		if wi.w.Token != "" {
			continue
		}
		key := fmt.Sprintf("%s|%s|%d", wi.w.URL, wi.w.Sitekey, i)
		res := p.resolveWidget(ctx, wi, key, o, inPage, len(ws) == 1)
		attempts += res.Attempts
		solverUsed = solverUsed || res.SolverUsed
		visualUsed = visualUsed || res.VisualUsed
		visualMS += res.VisualMS
		cost += res.CostUSD
		if res.State != turnstile.StateSolved {
			reasons = append(reasons, res.Reason)
		}
		if ctx.Err() != nil {
			break
		}
	}

	finalWs, finalInterstitial, derr := p.detectTurnstiles(ctx)
	if derr != nil || finalInterstitial || len(finalWs) == 0 {
		final := initial
		if finalInterstitial {
			final = &turnstile.Status{Detected: true, State: turnstile.StateInterstitial, Reason: "full-page Cloudflare challenge"}
		} else if len(finalWs) == 0 {
			final.State = turnstile.StateUnresolved
			final.Reason = "widgets disappeared after resolution"
		}
		final.Attempts, final.SolverUsed, final.VisualUsed, final.VisualMS, final.CostUSD = attempts, solverUsed, visualUsed, visualMS, cost
		if final.State != turnstile.StateSolved && len(reasons) > 0 {
			final.Reason = aggregateTurnstileReason(final, reasons[0])
		}
		return final
	}
	final := statusOfMany(finalWs)
	final.Attempts, final.SolverUsed, final.VisualUsed, final.VisualMS, final.CostUSD = attempts, solverUsed, visualUsed, visualMS, cost
	if final.State != turnstile.StateSolved && len(reasons) > 0 {
		final.Reason = aggregateTurnstileReason(final, reasons[0])
	}
	return final
}

func aggregateTurnstileReason(st *turnstile.Status, reason string) string {
	if st.Widgets > 1 {
		return fmt.Sprintf("%d of %d widgets unsolved: %s", st.Unsolved, st.Widgets, reason)
	}
	return reason
}

// resolveWidget runs the attempt bookkeeping, in-page click, opt-in visual
// phase and solver for one widget.
func (p *Page) resolveWidget(ctx context.Context, wi *tsWidgetInfo, key string, o turnstileOptions, inPage time.Duration, allowReset bool) *turnstile.Status {
	w, wf, inner := wi.w, wi.frame, wi.inner
	st := statusOf(w, inner)
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
	st.Reason = ""

	if p.tryTurnstileInPage(ctx, w, wf, inner, inPage, allowReset) {
		delete(p.tsTries, key)
		return &turnstile.Status{
			Detected: true, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData,
			Mode: w.Mode, State: turnstile.StateSolved, Attempts: a.cycles,
		}
	}

	visualReason := ""
	if o.Visual != nil {
		vb := o.VisualBudget
		if vb <= 0 {
			vb = 8 * time.Second
		}
		ok, reason, used, cost, ms := p.tryTurnstileVisual(ctx, o.Visual, vb, w.Index)
		st.VisualUsed = used
		st.VisualMS = ms
		st.CostUSD += cost
		if ok {
			delete(p.tsTries, key)
			return &turnstile.Status{
				Detected: true, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData,
				Mode: w.Mode, State: turnstile.StateSolved, Attempts: a.cycles,
				VisualUsed: true, VisualMS: ms, CostUSD: st.CostUSD,
			}
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
	if err := p.injectTurnstileTokenAt(ctx, w.Index, token); err != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "token injection failed: " + err.Error()
		return st
	}
	if p.turnstileTokenAt(ctx, w.Index) == "" {
		st.State = turnstile.StateUnresolved
		st.Reason = "solver returned a token but the page did not accept it"
		return st
	}
	delete(p.tsTries, key)
	return &turnstile.Status{
		Detected: true, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData,
		Mode: w.Mode, State: turnstile.StateSolved, Attempts: a.cycles,
		SolverUsed: true, VisualUsed: st.VisualUsed, VisualMS: st.VisualMS, CostUSD: st.CostUSD,
	}
}

func detectWidgetAt(ws []*tsWidgetInfo, i int) *tsWidgetInfo {
	if i >= 0 && i < len(ws) {
		return ws[i]
	}
	return nil
}

func (p *Page) turnstileTokenAt(ctx context.Context, i int) string {
	v, err := p.Eval(ctx, turnstileTokenAtJS(i))
	if err != nil {
		return ""
	}
	var s string
	if jsonv2.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

func (p *Page) tryTurnstileInPage(ctx context.Context, w *tsWidget, wf *frame, inner *tsInner, budget time.Duration, allowReset bool) bool {
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
			p.clickTurnstileWidget(ctx, w)
		}
	}
	click()
	lastDetect := start
	retried := false
	for {
		if p.turnstileTokenAt(ctx, w.Index) != "" {
			return true
		}
		if time.Since(start) > budget {
			final, fcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			ok := p.turnstileTokenAt(final, w.Index) != ""
			fcancel()
			return ok
		}
		// The script renders the widget iframe after the root appears; pick
		// up a checkbox that shows up during the wait.
		if time.Since(lastDetect) > 750*time.Millisecond {
			lastDetect = time.Now()
			if fresh, _, err := p.detectTurnstiles(ctx); err == nil {
				if fw := detectWidgetAt(fresh, w.Index); fw != nil {
					w.Widget, w.Root, w.Mode = fw.w.Widget, fw.w.Root, fw.w.Mode
					wf, inner = fw.frame, fw.inner
					if inner != nil && inner.Checkbox != nil && !clickedCheckbox {
						click()
					}
				}
			}
		}
		if !retried && time.Since(start) > 3*time.Second {
			retried = true
			if allowReset && inner != nil && turnstileWidgetError(inner.Text) {
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

func (p *Page) tryTurnstileVisual(ctx context.Context, v turnstileVisual, budget time.Duration, index int) (bool, string, bool, float64, int64) {
	if budget <= 0 {
		budget = 8 * time.Second
	}
	// A background tab can stall Page.captureScreenshot on headless Linux
	// even when focus emulation reports it visible.
	_, _ = cdproto.Call(ctx, p.S, cdppage.BringToFront, cdproto.Empty{})
	start := time.Now()
	clip, err := p.turnstileVisualClip(ctx, index)
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
		if p.turnstileTokenAt(ctx, index) != "" {
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
		if p.turnstileTokenAt(ctx, index) != "" {
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
func (p *Page) turnstileVisualClip(ctx context.Context, index int) (Clip, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return Clip{}, err
	}
	if len(fs) == 0 {
		return Clip{}, errors.New("no frames")
	}
	_ = p.eval(ctx, fs[0], turnstileScrollAtJS(index), nil)
	fresh, _, err := p.detectTurnstiles(ctx)
	if err != nil {
		return Clip{}, err
	}
	fw := detectWidgetAt(fresh, index)
	if fw == nil {
		return Clip{}, errors.New("widget disappeared")
	}
	w := fw.w
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

func (p *Page) clickTurnstileWidget(ctx context.Context, w *tsWidget) {
	fs, err := p.frames(ctx)
	if err != nil || len(fs) == 0 {
		return
	}
	_ = p.eval(ctx, fs[0], turnstileScrollAtJS(w.Index), nil)
	if fresh, _, err := p.detectTurnstiles(ctx); err == nil {
		if fw := detectWidgetAt(fresh, w.Index); fw != nil {
			w.Widget = fw.w.Widget
		}
	}
	if w.Widget == nil {
		return
	}
	x := w.Widget.X + math.Min(24, w.Widget.W*0.25)
	y := w.Widget.Y + w.Widget.H/2
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

func (p *Page) injectTurnstileTokenAt(ctx context.Context, index int, token string) error {
	js := fmt.Sprintf(`(() => {
	  const token = %s;
	  const top = %s;
	  const root = top[%d];
	  const sel = 'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]';
	  let el = root ? (root.querySelector(sel) || (top.length === 1 ? document.querySelector(sel) : null)) : document.querySelector(sel);
	  if (!el) {
	    el = document.createElement('input');
	    el.type = 'hidden';
	    el.name = 'cf-turnstile-response';
	    (root || document.body).appendChild(el);
	  }
	  const proto = el.localName === 'textarea' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
	  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, token);
	  el.dispatchEvent(new Event('input', {bubbles:true, composed:true}));
	  el.dispatchEvent(new Event('change', {bubbles:true}));
	  return el.value;
	})()`, jsString(token), turnstileRootsJS, index)
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
