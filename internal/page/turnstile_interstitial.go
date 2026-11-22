package page

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/barockok/brojev/internal/turnstile"
)

// Full-page Cloudflare challenges ("Just a moment...", "Performing security
// verification") draw their checkbox in a challenges.cloudflare.com iframe
// that usually sits in a closed shadow root, with none of the widget markers
// detectTurnstiles looks for in the top document. They pass by leaving the
// challenge page (Cloudflare sets its clearance cookie and reloads), not by a
// token in a form, so a CapSolver token cannot clear them.
//
// resolveInterstitial waits for the challenge frame (the page may also pass on
// its own while it waits), clicks its checkbox (or the frame's left-center
// when the checkbox is out of reach), then tries the opt-in visual phase on a
// clip around the frame. Success is the challenge page going away.

// challengeFrame returns the frame that draws the challenge and its box in
// viewport coordinates: a visible challenges.cloudflare.com frame first, else
// the largest visible child frame (local fixtures, proxied challenges).
func (p *Page) challengeFrame(ctx context.Context) (*frame, *tsRect, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(fs) < 2 {
		return nil, nil, nil
	}
	origin, clip := p.offsets(ctx, fs)
	var best, cf *frame
	var bestArea float64
	for _, f := range fs[1:] {
		c, ok := clip[f.ID]
		if !ok || c.W < 20 || c.H < 20 {
			continue
		}
		if cf == nil && strings.Contains(f.URL, "challenges.cloudflare.com") {
			cf = f
		}
		if a := c.W * c.H; a > bestArea {
			best, bestArea = f, a
		}
	}
	if cf != nil {
		best = cf
	}
	if best == nil {
		return nil, nil, nil
	}
	o := origin[best.ID]
	return best, &tsRect{X: o.X, Y: o.Y, W: o.W, H: o.H}, nil
}

// interstitialShown reports whether the page still shows a full-page
// challenge. A failed read (the page is navigating) counts as still shown.
func (p *Page) interstitialShown(ctx context.Context) bool {
	_, inter, err := p.detectTurnstiles(ctx)
	return err != nil || inter
}

// waitInterstitialGone polls until the challenge page is gone or d passes.
func (p *Page) waitInterstitialGone(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !p.interstitialShown(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func interstitialKey(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return "interstitial|" + u.Host
	}
	return "interstitial|" + raw
}

func (p *Page) currentURL(ctx context.Context) string {
	fs, err := p.frames(ctx)
	if err != nil || len(fs) == 0 {
		return ""
	}
	return fs[0].URL
}

func (p *Page) resolveInterstitial(ctx context.Context, o turnstileOptions, inPage time.Duration) *turnstile.Status {
	st := &turnstile.Status{Detected: true, State: turnstile.StateInterstitial, Mode: turnstile.ModeInterstitial}
	passed := func() *turnstile.Status {
		return &turnstile.Status{
			Detected: true, State: turnstile.StateSolved, Mode: turnstile.ModeInterstitial,
			Attempts: st.Attempts, VisualUsed: st.VisualUsed, VisualMS: st.VisualMS, CostUSD: st.CostUSD,
		}
	}
	if p.tsTries == nil || len(p.tsTries) > 128 {
		p.tsTries = map[string]tsAttempt{}
	}
	key := interstitialKey(p.currentURL(ctx))
	a := p.tsTries[key]
	st.Attempts = a.cycles
	if a.cycles >= 2 {
		st.Reason = "gave up after 2 attempts"
		return st
	}
	if !a.last.IsZero() && time.Since(a.last) < time.Second {
		st.Reason = "cooling down"
		return st
	}
	a.cycles++
	a.last = time.Now()
	p.tsTries[key] = a
	st.Attempts = a.cycles
	done := func() *turnstile.Status {
		delete(p.tsTries, key)
		return passed()
	}

	// 1. The challenge script renders its frame a moment after the page; many
	// challenges also pass on their own here, without a click.
	_ = p.Front(ctx)
	var cf *frame
	var fr *tsRect
	deadline := time.Now().Add(inPage)
	for {
		if !p.interstitialShown(ctx) {
			return done()
		}
		if f, r, err := p.challengeFrame(ctx); err == nil && f != nil {
			cf, fr = f, r
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			st.Reason = "timed out"
			return st
		case <-time.After(250 * time.Millisecond):
		}
	}

	// 2. In-page click: the checkbox inside the frame when it is reachable,
	// else the frame's left-center, where Cloudflare draws it.
	var reasons []string
	if cf == nil {
		reasons = append(reasons, "challenge frame not found")
	} else {
		click := func() {
			var inner tsInner
			if p.eval(ctx, cf, turnstileWidgetJS, &inner) == nil && inner.Checkbox != nil {
				if p.clickTurnstileCheckbox(ctx, cf, inner.Checkbox) == nil {
					return
				}
			}
			if f, r, err := p.challengeFrame(ctx); err == nil && f != nil {
				cf, fr = f, r
			}
			x := fr.X + math.Min(24, fr.W*0.25)
			y := fr.Y + fr.H/2
			_, _ = p.ClickXY(ctx, x, y, ClickOptions{})
		}
		click()
		if p.waitInterstitialGone(ctx, inPage/2) {
			return done()
		}
		click() // the checkbox often appears only after a first spinner
		if p.waitInterstitialGone(ctx, inPage/2) {
			return done()
		}
		reasons = append(reasons, "in-page click did not pass")
	}

	// 3. Opt-in visual phase on a clip around the frame (or the middle of the
	// viewport when no frame is reachable: the challenge draws there).
	if o.Visual != nil {
		vb := o.VisualBudget
		if vb <= 0 {
			vb = 8 * time.Second
		}
		clip, err := p.interstitialClip(ctx, fr)
		if err != nil {
			reasons = append(reasons, "visual: clip failed: "+err.Error())
		} else {
			ok, reason, used, cost, ms := p.tryVisualClick(ctx, o.Visual, vb, clip, func(c context.Context) bool { return !p.interstitialShown(c) }, "challenge page still shown")
			st.VisualUsed, st.VisualMS = used, ms
			st.CostUSD += cost
			if ok {
				return done()
			}
			reasons = append(reasons, "visual: "+reason)
		}
	} else if o.VisualNote != "" {
		reasons = append(reasons, o.VisualNote)
	}
	if !p.interstitialShown(ctx) {
		return done()
	}
	st.Reason = strings.Join(reasons, "; ")
	return st
}

// interstitialClip frames the challenge frame plus context, or the middle of
// the viewport when there is no frame, within the visual API's size window.
func (p *Page) interstitialClip(ctx context.Context, fr *tsRect) (Clip, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return Clip{}, err
	}
	if len(fs) == 0 {
		return Clip{}, errors.New("no frames")
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
	base := fr
	if base == nil || base.W <= 0 || base.H <= 0 {
		w, h := math.Min(vp.W, 900), math.Min(vp.H, 500)
		base = &tsRect{X: (vp.W - w) / 2, Y: (vp.H - h) / 2, W: w, H: h}
	}
	return clipAround(base, vp.W, vp.H)
}
