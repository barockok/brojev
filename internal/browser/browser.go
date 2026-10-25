// Package browser holds brojev's view of Chrome: tabs and their flat sessions.
package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cbrowser "github.com/chromedp/cdproto/browser"
	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/cdp"
)

type Browser struct {
	Endpoint string
	conn     *cdp.Conn

	mu    sync.Mutex
	tabs  map[target.ID]*Tab
	dl    *downloads
	tabMu sync.Mutex // serializes creating and closing tabs
	guard target.ID  // see EnsureGuard; hidden from Pages, never attached or closed
}

// GuardURL marks the guard tab, so a restarted daemon adopts it instead of
// adding another.
const GuardURL = "about:blank#brojev-guard"

var ErrGuardTab = errors.New("that is brojev's guard tab: it keeps the external Chrome's window open, so brojev does not drive or close it")

// EnsureGuard keeps exactly one guard tab open (BROJEV_CDP_URL mode): an
// idle about:blank tab brojev owns, so closing every other tab (or a page
// calling window.close()) never closes the last window of a Chrome brojev
// does not manage. It adopts an existing guard, recreates a closed one, and
// closes extra guards.
func (b *Browser) EnsureGuard(ctx context.Context) error {
	b.tabMu.Lock()
	defer b.tabMu.Unlock()
	res, err := cdproto.Call(ctx, b.conn.Browser(), target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return err
	}
	b.mu.Lock()
	cur := b.guard
	b.mu.Unlock()
	var guards []target.ID
	for _, t := range res.TargetInfos {
		if t.Type == "page" && (t.TargetID == cur || t.URL == GuardURL) {
			if t.TargetID == cur { // the recorded one wins
				guards = append([]target.ID{t.TargetID}, guards...)
			} else {
				guards = append(guards, t.TargetID)
			}
		}
	}
	if len(guards) == 0 {
		no, yes := false, true
		created, err := cdproto.Call(ctx, b.conn.Browser(), target.CreateTarget, target.CreateTargetParams{URL: GuardURL, Background: &yes, Focus: &no})
		if err != nil {
			return fmt.Errorf("guard tab: %w", err)
		}
		guards = []target.ID{created.TargetID}
	}
	for _, id := range guards[1:] {
		_, _ = cdproto.Call(ctx, b.conn.Browser(), target.CloseTarget, target.CloseTargetParams{TargetID: id})
	}
	b.mu.Lock()
	b.guard = guards[0]
	b.mu.Unlock()
	return nil
}

func (b *Browser) isGuard(id target.ID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.guard != "" && id == b.guard
}

type Tab struct {
	ID      target.ID
	Session *cdp.Session
}

func Connect(ctx context.Context, endpoint string) (*Browser, error) {
	conn, err := cdp.Dial(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return &Browser{Endpoint: endpoint, conn: conn, tabs: map[target.ID]*Tab{}}, nil
}

func (b *Browser) Done() <-chan struct{} { return b.conn.Done() }
func (b *Browser) Conn() *cdp.Conn       { return b.conn }
func (b *Browser) Close() error          { return b.conn.Close() }

func (b *Browser) Version(ctx context.Context) (*cbrowser.GetVersionResult, error) {
	v, err := cdproto.Call(ctx, b.conn.Browser(), cbrowser.GetVersion, cdproto.Empty{})
	return &v, err
}

// Pages lists page targets (tabs) in the browser, without the guard tab.
func (b *Browser) Pages(ctx context.Context) ([]*target.Info, error) {
	res, err := cdproto.Call(ctx, b.conn.Browser(), target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return nil, err
	}
	var pages []*target.Info
	for _, t := range res.TargetInfos {
		if t.Type == "page" && !b.isGuard(t.TargetID) {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// Attach returns the tab's session, attaching once per target.
func (b *Browser) Attach(ctx context.Context, id target.ID) (*Tab, error) {
	if b.isGuard(id) {
		return nil, ErrGuardTab
	}
	b.mu.Lock()
	if t := b.tabs[id]; t != nil {
		b.mu.Unlock()
		return t, nil
	}
	b.mu.Unlock()
	flat := true
	res, err := cdproto.Call(ctx, b.conn.Browser(), target.AttachToTarget, target.AttachToTargetParams{TargetID: id, Flatten: &flat})
	if err != nil {
		return nil, err
	}
	t := &Tab{ID: id, Session: b.conn.Session(string(res.SessionID))}
	if _, err := cdproto.Call(ctx, t.Session, page.Enable, page.EnableParams{}); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if prev := b.tabs[id]; prev != nil { // lost a race; keep the first
		b.mu.Unlock()
		return prev, nil
	}
	b.tabs[id] = t
	b.mu.Unlock()
	return t, nil
}

type PageInfo struct {
	TargetID string `json:"targetId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	LoadMS   int64  `json:"loadMs"`
}

// Open creates a background tab, navigates it and waits for load.
func (b *Browser) Open(ctx context.Context, url string) (*PageInfo, error) {
	no := false
	b.tabMu.Lock()
	created, err := cdproto.Call(ctx, b.conn.Browser(), target.CreateTarget, target.CreateTargetParams{URL: "about:blank", Focus: &no})
	b.tabMu.Unlock()
	if err != nil {
		return nil, err
	}
	t, err := b.Attach(ctx, created.TargetID)
	if err != nil {
		return nil, err
	}
	ms, err := t.Navigate(ctx, url)
	if err != nil {
		return nil, err
	}
	info, err := t.Info(ctx)
	if err != nil {
		return nil, err
	}
	info.LoadMS = ms
	return info, nil
}

// Forget drops a tab's cached session (after it closed).
func (b *Browser) Forget(id target.ID) {
	b.mu.Lock()
	delete(b.tabs, id)
	b.mu.Unlock()
}

func (b *Browser) CloseTab(ctx context.Context, id target.ID) error {
	if b.isGuard(id) {
		return ErrGuardTab
	}
	b.tabMu.Lock()
	_, err := cdproto.Call(ctx, b.conn.Browser(), target.CloseTarget, target.CloseTargetParams{TargetID: id})
	b.tabMu.Unlock()
	b.mu.Lock()
	delete(b.tabs, id)
	b.mu.Unlock()
	return err
}

// Navigate loads url and waits for the load event. Returns elapsed ms.
func (t *Tab) Navigate(ctx context.Context, url string) (int64, error) {
	start := time.Now()
	// Subscribe before navigating so the event cannot be missed.
	loaded, cancel := t.Session.Subscribe(page.LoadEventFired.Method)
	defer cancel()
	nav, err := cdproto.Call(ctx, t.Session, page.Navigate, page.NavigateParams{URL: url})
	if err != nil {
		return 0, err
	}
	if nav.ErrorText != "" {
		return 0, fmt.Errorf("navigate %s: %s", url, nav.ErrorText)
	}
	if nav.LoaderID == "" { // same-document navigation fires no load event
		return time.Since(start).Milliseconds(), nil
	}
	wait, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	defer cancelWait()
	select {
	case <-loaded:
	case <-wait.Done():
		return 0, fmt.Errorf("navigate %s: load event: %w", url, wait.Err())
	}
	return time.Since(start).Milliseconds(), nil
}

// Eval evaluates expression and decodes its JSON value into out.
func (t *Tab) Eval(ctx context.Context, expression string, out any) error {
	yes := true
	res, err := cdproto.Call(ctx, t.Session, runtime.Evaluate, runtime.EvaluateParams{
		Expression: expression, ReturnByValue: &yes, AwaitPromise: &yes,
	})
	if err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			msg = res.ExceptionDetails.Exception.Description
		}
		return fmt.Errorf("eval: %s", msg)
	}
	if out == nil || res.Result == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return jsonv2.Unmarshal(res.Result.Value, out)
}

func (t *Tab) Info(ctx context.Context) (*PageInfo, error) {
	var v struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := t.Eval(ctx, `({url: location.href, title: document.title})`, &v); err != nil {
		return nil, err
	}
	return &PageInfo{TargetID: string(t.ID), URL: v.URL, Title: v.Title}, nil
}
