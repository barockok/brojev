package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/page"
)

// Handoff lets a person take over one step (a login, a CAPTCHA, a 2FA prompt)
// in a visible Chrome while the agent keeps driving its own browser, which may
// be headless Chrome or a CDP engine like Obscura. start copies the agent's
// cookies and localStorage into the person's Chrome and opens the same URL
// there; finish copies them back and moves the agent's tab to where the person
// ended up. The person's Chrome is never launched or quit by brojev.
//
// It is off unless BROJEV_HUMAN_CDP_URL is set; nothing else changes then.

// HumanCDPURL is BROJEV_HUMAN_CDP_URL: the DevTools HTTP base of the visible
// Chrome a person watches (e.g. through noVNC, see scripts/human-browser.sh).
func HumanCDPURL() string { return os.Getenv("BROJEV_HUMAN_CDP_URL") }

// HumanViewURL is BROJEV_HUMAN_VIEW_URL: the link the person opens to see
// that Chrome (e.g. a noVNC page). Optional; only reported back.
func HumanViewURL() string { return os.Getenv("BROJEV_HUMAN_VIEW_URL") }

var errNoHuman = errors.New("handoff: BROJEV_HUMAN_CDP_URL is not set (point it at a visible Chrome the person can see, e.g. started by scripts/human-browser.sh)")

type handoffParams struct {
	Action      string `json:"action"`
	URL         string `json:"url,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Snapshot    bool   `json:"snapshot,omitempty"`
	MaxElements int    `json:"maxElements,omitempty"`
	MaxText     int    `json:"maxText,omitempty"`
}

// handoffState is the open handoff: the tab brojev opened in the person's
// Chrome. Guarded by Server.mu.
type handoffState struct {
	Tab     target.ID `json:"tab"`
	URL     string    `json:"url"`
	Reason  string    `json:"reason,omitempty"`
	Started time.Time `json:"started"`
}

// transferred is what one direction of a handoff copied.
type transferred struct {
	Cookies       int    `json:"cookies"`
	CookiesFailed int    `json:"cookiesFailed,omitempty"`
	LocalStorage  int    `json:"localStorage,omitempty"`
	Origin        string `json:"origin,omitempty"`
}

func (s *Server) handoffTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var a handoffParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("handoff: bad params: %w", err)
		}
	}
	base := HumanCDPURL()
	if base == "" {
		return nil, errNoHuman
	}
	switch a.Action {
	case "start":
		return s.handoffStart(ctx, base, a)
	case "finish":
		return s.handoffFinish(ctx, base, a)
	case "", "status":
		return s.handoffStatus(ctx, base)
	}
	return nil, errors.New("handoff: action must be start, finish or status")
}

func connectHuman(ctx context.Context, base string) (*browser.Browser, error) {
	ep, err := chrome.EndpointFromURL(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("handoff: person's Chrome: %w", err)
	}
	return browser.Connect(ctx, ep)
}

func (s *Server) handoffStart(ctx context.Context, base string, a handoffParams) (_ any, err error) {
	// Reserve the slot so concurrent starts cannot both open one.
	st := &handoffState{Reason: a.Reason, Started: time.Now()}
	s.mu.Lock()
	open := s.handoff
	if open == nil {
		s.handoff = st
	}
	s.mu.Unlock()
	if open != nil {
		return nil, fmt.Errorf("handoff: one is already open (%s since %s); finish it first", open.URL, open.Started.Format(time.Kitchen))
	}
	defer func() {
		if err != nil {
			s.clearHandoff()
		}
	}()
	b, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.current(ctx, b)
	if err != nil {
		return nil, err
	}
	dest := a.URL
	if dest == "" {
		dest = p.CurrentURL(ctx)
	}
	if !strings.HasPrefix(dest, "http://") && !strings.HasPrefix(dest, "https://") {
		return nil, fmt.Errorf("handoff: no http(s) page to hand over (current: %q); pass url", dest)
	}

	// Read the agent's state before touching the person's Chrome.
	var res struct {
		Cookies []map[string]any `json:"cookies"`
	}
	if err := p.S.Conn().Browser().Call(ctx, "Storage.getCookies", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("handoff: read cookies: %w", err)
	}
	ls := map[string]string{}
	origin := p.Origin(ctx)
	if origin != "" && origin == originOf(dest) {
		if v, err := p.LocalStorage(ctx); err == nil {
			ls = v
		}
	}

	hb, err := connectHuman(ctx, base)
	if err != nil {
		return nil, err
	}
	defer hb.Close()
	sent := transferred{Origin: originOf(dest)}
	sent.Cookies, sent.CookiesFailed = setCookies(ctx, hb.Conn().Browser(), res.Cookies)
	info, err := hb.Open(ctx, dest)
	if err != nil {
		return nil, fmt.Errorf("handoff: open in person's Chrome: %w", err)
	}
	tab, err := hb.Attach(ctx, target.ID(info.TargetID))
	if err != nil {
		return nil, err
	}
	if len(ls) > 0 {
		if err := tabSetLocalStorage(ctx, tab, ls); err == nil {
			sent.LocalStorage = len(ls)
			if _, err := tab.Navigate(ctx, dest); err != nil { // reload so the page sees it
				log.Printf("handoff: reload after localStorage: %v", err)
			}
		}
	}
	tabFront(ctx, tab)

	s.mu.Lock()
	st.Tab, st.URL = target.ID(info.TargetID), dest
	s.mu.Unlock()
	log.Printf("handoff started: %s (%d cookies)", dest, sent.Cookies)
	out := map[string]any{
		"status": "waiting_for_person", "url": dest, "sent": sent,
		"next": "ask the person to finish the step in their browser, then call handoff with action=finish",
	}
	if v := HumanViewURL(); v != "" {
		out["viewUrl"] = v
	}
	if a.Reason != "" {
		out["reason"] = a.Reason
	}
	return out, nil
}

func (s *Server) handoffFinish(ctx context.Context, base string, a handoffParams) (any, error) {
	s.mu.Lock()
	st := s.handoff
	s.mu.Unlock()
	if st == nil {
		return nil, errors.New("handoff: none open; call handoff with action=start first")
	}
	hb, err := connectHuman(ctx, base)
	if err != nil {
		return nil, err
	}
	defer hb.Close()
	tab, err := hb.Attach(ctx, st.Tab)
	if err != nil {
		s.clearHandoff()
		return nil, fmt.Errorf("handoff: the person's tab is gone (%v); handoff closed, start again if needed", err)
	}
	info, err := tab.Info(ctx)
	if err != nil {
		return nil, err
	}
	var res struct {
		Cookies []map[string]any `json:"cookies"`
	}
	if err := hb.Conn().Browser().Call(ctx, "Storage.getCookies", map[string]any{}, &res); err != nil {
		return nil, fmt.Errorf("handoff: read person's cookies: %w", err)
	}
	ls, _ := tabLocalStorage(ctx, tab)
	dest, formResult := info.URL, false
	if a.URL != "" {
		dest = a.URL
	} else {
		formResult = lastEntryIsForm(ctx, tab)
	}

	b, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.current(ctx, b)
	if err != nil {
		return nil, err
	}
	got := transferred{Origin: originOf(dest)}
	got.Cookies, got.CookiesFailed = setCookies(ctx, p.S.Conn().Browser(), res.Cookies)
	before, _ := b.Pages(ctx)
	r, err := p.Navigate(ctx, dest)
	if err != nil {
		return nil, fmt.Errorf("handoff: agent navigate to %s: %w", dest, err)
	}
	if len(ls) > 0 && p.Origin(ctx) == got.Origin {
		if err := p.SetLocalStorage(ctx, ls); err == nil {
			got.LocalStorage = len(ls)
			if rr, err := p.Reload(ctx); err == nil {
				r = rr
			}
		}
	}
	closeHumanTab(ctx, hb, st.Tab)
	s.clearHandoff()
	log.Printf("handoff finished: %s (%d cookies back)", dest, got.Cookies)
	r.Note = strings.TrimSpace(fmt.Sprintf("handoff finished: copied %d cookies and %d localStorage keys back from the person's browser. %s", got.Cookies, got.LocalStorage, r.Note))
	if formResult {
		r.Note += " The person ended on a form submission result; it was loaded again with GET, so navigate to the page you need."
	}
	reply, err := s.reply(ctx, b, p, r, before, Params{Snapshot: a.Snapshot, MaxElements: a.MaxElements, MaxText: a.MaxText})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "finished", "received": got, "page": reply}, nil
}

func (s *Server) handoffStatus(ctx context.Context, base string) (any, error) {
	s.mu.Lock()
	st := s.handoff
	s.mu.Unlock()
	out := map[string]any{"open": st != nil, "humanCdpUrl": chrome.RedactURL(base)}
	if v := HumanViewURL(); v != "" {
		out["viewUrl"] = v
	}
	if st == nil {
		return out, nil
	}
	out["startedUrl"], out["reason"], out["sinceSec"] = st.URL, st.Reason, int(time.Since(st.Started).Seconds())
	if hb, err := connectHuman(ctx, base); err == nil {
		defer hb.Close()
		if tab, err := hb.Attach(ctx, st.Tab); err == nil {
			if info, err := tab.Info(ctx); err == nil {
				out["personUrl"], out["personTitle"] = info.URL, info.Title
			}
		} else {
			out["personTab"] = "gone"
		}
	}
	return out, nil
}

func (s *Server) clearHandoff() {
	s.mu.Lock()
	s.handoff = nil
	s.mu.Unlock()
}

// caller is the browser-level CDP session (cdp.Session satisfies it).
type caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// setCookies copies cookies read from one browser into another. Engines
// differ in which fields and enum values they accept, so a rejected batch is
// retried one cookie at a time and the failures are counted, not fatal.
func setCookies(ctx context.Context, to caller, raw []map[string]any) (ok, failed int) {
	cookies := page.CleanCookies(raw)
	if len(cookies) == 0 {
		return 0, 0
	}
	if to.Call(ctx, "Storage.setCookies", map[string]any{"cookies": cookies}, nil) == nil {
		return len(cookies), 0
	}
	for _, c := range cookies {
		err := to.Call(ctx, "Storage.setCookies", map[string]any{"cookies": []map[string]any{c}}, nil)
		if err != nil { // retry with the fields every engine knows
			min := map[string]any{}
			for _, f := range []string{"name", "value", "domain", "path", "secure", "httpOnly", "expires"} {
				if v, ok := c[f]; ok {
					min[f] = v
				}
			}
			err = to.Call(ctx, "Storage.setCookies", map[string]any{"cookies": []map[string]any{min}}, nil)
		}
		if err != nil {
			failed++
			log.Printf("handoff: cookie %v@%v not copied: %v", c["name"], c["domain"], err)
		} else {
			ok++
		}
	}
	return ok, failed
}

func tabLocalStorage(ctx context.Context, t *browser.Tab) (map[string]string, error) {
	var ls map[string]string
	err := t.Eval(ctx, "Object.fromEntries(Object.entries(localStorage))", &ls)
	return ls, err
}

func tabSetLocalStorage(ctx context.Context, t *browser.Tab, ls map[string]string) error {
	b, _ := json.Marshal(ls)
	return t.Eval(ctx, fmt.Sprintf("(() => { const d = %s; for (const k in d) localStorage.setItem(k, d[k]); return Object.keys(d).length })()", b), nil)
}

// tabFront raises the handoff tab so the person sees it.
func tabFront(ctx context.Context, t *browser.Tab) {
	if err := t.Session.Call(ctx, "Page.bringToFront", map[string]any{}, nil); err != nil {
		log.Printf("handoff: bring to front: %v", err)
	}
}

// closeHumanTab closes the handoff tab unless it is the last one: closing a
// visible Chrome's last tab would close its window.
func closeHumanTab(ctx context.Context, hb *browser.Browser, id target.ID) {
	pages, err := hb.Pages(ctx)
	if err != nil {
		return
	}
	if len(pages) > 1 {
		_ = hb.CloseTab(ctx, id)
		return
	}
	if tab, err := hb.Attach(ctx, id); err == nil {
		_, _ = tab.Navigate(ctx, "about:blank")
	}
}

// lastEntryIsForm reports whether the tab's current history entry came from
// a form submission, which a plain navigation cannot reproduce.
func lastEntryIsForm(ctx context.Context, t *browser.Tab) bool {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			TransitionType string `json:"transitionType"`
		} `json:"entries"`
	}
	if err := t.Session.Call(ctx, "Page.getNavigationHistory", map[string]any{}, &h); err != nil {
		return false
	}
	return h.CurrentIndex >= 0 && h.CurrentIndex < len(h.Entries) && h.Entries[h.CurrentIndex].TransitionType == "form_submit"
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
