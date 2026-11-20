package page

import (
	"context"
	"strings"

	cdproto "github.com/chromedp/cdproto/cdp"

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
	if len(fs) > 1 {
		origin, clip := p.offsets(ctx, fs)
		wf = pickTurnstileFrame(fs, origin, clip, w.Root)
		if wf != nil {
			o, ok := origin[wf.ID]
			if ok {
				w.Widget = &tsRect{X: o.X, Y: o.Y, W: o.W, H: o.H}
			}
			c := clip[wf.ID]
			if c.W > 0 && c.H > 0 {
				w.Mode = "managed"
			} else {
				w.Mode = "invisible"
			}
		}
	}
	if w.Mode == "" {
		w.Mode = "invisible"
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
