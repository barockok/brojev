package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/agent"
	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/jev"
	"github.com/barockok/brojev/internal/llm"
	"github.com/barockok/brojev/internal/page"
)

// Params is the union of all tool parameters; each tool reads what it needs.
type Params struct {
	URL         string   `json:"url,omitempty"`
	Ref         string   `json:"ref,omitempty"`
	X           *float64 `json:"x,omitempty"`
	Y           *float64 `json:"y,omitempty"`
	X2          *float64 `json:"x2,omitempty"`
	Y2          *float64 `json:"y2,omitempty"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	DX          float64  `json:"dx,omitempty"`
	DY          float64  `json:"dy,omitempty"`
	Text        string   `json:"text,omitempty"`
	Values      []string `json:"values,omitempty"`
	Files       []string `json:"files,omitempty"`
	Combo       string   `json:"combo,omitempty"`
	Button      string   `json:"button,omitempty"`
	Count       int      `json:"count,omitempty"`
	Modifiers   []string `json:"modifiers,omitempty"`
	Append      bool     `json:"append,omitempty"`
	Submit      bool     `json:"submit,omitempty"`
	Full        bool     `json:"full,omitempty"`
	Gone        bool     `json:"gone,omitempty"`
	Accept      *bool    `json:"accept,omitempty"`
	TimeoutMS   int      `json:"timeoutMs,omitempty"`
	TargetID    string   `json:"targetId,omitempty"`
	JS          string   `json:"js,omitempty"`
	Snapshot    bool     `json:"snapshot,omitempty"` // attach a fresh snapshot to action results
	MaxElements int      `json:"maxElements,omitempty"`
	Device      string   `json:"device,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Path        string   `json:"path,omitempty"`
	MaxText     int      `json:"maxText,omitempty"`
}

type TabInfo struct {
	TargetID string `json:"targetId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Current  bool   `json:"current,omitempty"`
}

// ActionReply is what every page action returns.
type ActionReply struct {
	*page.Result
	NewTabs  []TabInfo `json:"newTabs,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
}

var pageTools = map[string]bool{
	"navigate": true, "snapshot": true, "click": true, "hover": true, "type": true, "select": true,
	"key": true, "keytype": true, "scroll": true, "drag": true, "upload": true, "dialog": true,
	"screenshot": true, "eval": true, "wait": true, "back": true, "forward": true, "reload": true,
	"tab_new": true, "tab_switch": true, "tab_close": true, "tabs": true, "downloads": true,
	"emulate": true, "grant": true, "state_save": true, "state_load": true,
}

// current returns the page brojev is driving, attaching to a tab if needed.
func (s *Server) current(ctx context.Context, b *browser.Browser) (*page.Page, error) {
	s.mu.Lock()
	cur := s.cur
	p := s.pages[cur]
	s.mu.Unlock()
	pages, err := b.Pages(ctx)
	if err != nil {
		return nil, err
	}
	alive := false
	for _, t := range pages {
		if t.TargetID == cur {
			alive = true
		}
	}
	if p != nil && alive {
		return p, nil
	}
	if !alive {
		s.mu.Lock()
		attached := s.prof.Attach
		s.mu.Unlock()
		// On an attached (personal) browser never take over one of the
		// user's tabs: brojev works in a tab of its own.
		if len(pages) == 0 || attached {
			info, err := b.Open(ctx, "about:blank")
			if err != nil {
				return nil, err
			}
			cur = target.ID(info.TargetID)
		} else {
			cur = pages[0].TargetID
		}
	}
	return s.use(ctx, b, cur)
}

// use makes id the driven tab and brings it to the front: Chrome does not
// render background tabs, so their input is not hit-tested and their JS
// dialogs are deferred.
func (s *Server) use(ctx context.Context, b *browser.Browser, id target.ID) (*page.Page, error) {
	s.mu.Lock()
	if p := s.pages[id]; p != nil {
		changed := s.cur != id
		s.cur = id
		s.mu.Unlock()
		if changed {
			p.Front(ctx)
		}
		return p, nil
	}
	s.mu.Unlock()
	tab, err := b.Attach(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := page.New(ctx, id, tab.Session)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.pages == nil {
		s.pages = map[target.ID]*page.Page{}
	}
	if prev := s.pages[id]; prev != nil {
		p.Close()
		p = prev
	} else {
		s.pages[id] = p
	}
	s.cur = id
	s.mu.Unlock()
	p.Front(ctx)
	return p, nil
}

func (s *Server) tabs(ctx context.Context, b *browser.Browser) ([]TabInfo, error) {
	pages, err := b.Pages(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	out := make([]TabInfo, 0, len(pages))
	for _, t := range pages {
		out = append(out, TabInfo{TargetID: string(t.TargetID), URL: t.URL, Title: t.Title, Current: t.TargetID == cur})
	}
	return out, nil
}

func (s *Server) pageTool(ctx context.Context, b *browser.Browser, method string, raw json.RawMessage) (any, error) {
	var a Params
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("%s: bad params: %w", method, err)
		}
	}
	switch method { // tools that do not need a current page
	case "tabs":
		return s.tabs(ctx, b)
	case "downloads":
		return b.Downloads(), nil
	case "tab_new":
		url := a.URL
		if url == "" {
			url = "about:blank"
		}
		info, err := b.Open(ctx, url)
		if err != nil {
			return nil, err
		}
		if _, err := s.use(ctx, b, target.ID(info.TargetID)); err != nil {
			return nil, err
		}
		return info, nil
	case "tab_switch":
		if a.TargetID == "" {
			return nil, errors.New("tab_switch: targetId required")
		}
		if _, err := s.use(ctx, b, target.ID(a.TargetID)); err != nil {
			return nil, err
		}
		return s.tabs(ctx, b)
	case "tab_close":
		id := target.ID(a.TargetID)
		if id == "" {
			s.mu.Lock()
			id = s.cur
			s.mu.Unlock()
		}
		s.mu.Lock()
		if p := s.pages[id]; p != nil {
			p.Close()
			delete(s.pages, id)
		}
		if s.cur == id {
			s.cur = ""
		}
		s.mu.Unlock()
		if err := b.CloseTab(ctx, id); err != nil {
			return nil, err
		}
		return s.tabs(ctx, b)
	}

	p, err := s.current(ctx, b)
	if err != nil {
		return nil, err
	}
	switch method {
	case "snapshot":
		snap, err := p.Snapshot(ctx, page.SnapshotOptions{MaxElements: a.MaxElements, MaxText: a.MaxText})
		if err != nil {
			return nil, err
		}
		return map[string]any{"text": snap.String(), "url": snap.URL, "title": snap.Title, "elements": len(snap.Elements)}, nil
	case "screenshot":
		img, err := p.Screenshot(ctx, a.Full)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(Home(), "shots")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, time.Now().Format("20060102-150405.000")+".jpg")
		if err := os.WriteFile(path, img, 0o600); err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "bytes": len(img)}, nil
	case "eval":
		v, err := p.Eval(ctx, a.JS)
		if err != nil {
			return nil, err
		}
		return map[string]json.RawMessage{"value": json.RawMessage(orNull(v))}, nil
	case "wait":
		timeout := time.Duration(a.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		start := time.Now()
		if err := p.WaitText(ctx, a.Text, a.Gone, timeout); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "ms": time.Since(start).Milliseconds()}, nil
	case "emulate":
		var e page.Emulation
		if a.Device != "" {
			d, ok := page.Devices[a.Device]
			if !ok {
				names := make([]string, 0, len(page.Devices))
				for k := range page.Devices {
					names = append(names, k)
				}
				return nil, fmt.Errorf("emulate: unknown device %q (known: %s)", a.Device, strings.Join(names, ", "))
			}
			e = d
		}
		if len(raw) > 0 { // explicit fields override the preset
			if err := json.Unmarshal(raw, &e); err != nil {
				return nil, err
			}
		}
		if err := p.Emulate(ctx, e); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "applied": e}, nil
	case "grant":
		origin, err := p.Grant(ctx, a.Permissions)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "origin": origin, "permissions": a.Permissions}, nil
	case "state_save", "state_load":
		path := a.Path
		if path == "" {
			path = filepath.Join(Home(), "state.json")
		}
		if method == "state_save" {
			n, err := p.SaveState(ctx, path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"path": path, "cookies": n}, nil
		}
		note, err := p.LoadState(ctx, path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "note": note}, nil
	case "dialog":
		if a.Accept == nil {
			if d := p.OpenDialog(); d != nil {
				return d, nil
			}
			return nil, errors.New("dialog: no dialog open; pass accept=true|false to handle one")
		}
		d, err := p.HandleDialog(ctx, *a.Accept, a.Text)
		if err != nil {
			return nil, err
		}
		return s.reply(ctx, b, p, &page.Result{Note: fmt.Sprintf("%s %q handled (accept=%v)", d.Type, d.Message, *a.Accept)}, nil, a)
	}

	before, _ := b.Pages(ctx)
	var r *page.Result
	switch method {
	case "navigate":
		if a.URL == "" {
			return nil, errors.New("navigate: url required")
		}
		r, err = p.Navigate(ctx, a.URL)
	case "back":
		r, err = p.History(ctx, -1)
	case "forward":
		r, err = p.History(ctx, 1)
	case "reload":
		r, err = p.Reload(ctx)
	case "click":
		o := page.ClickOptions{Button: a.Button, Count: a.Count, Modifiers: a.Modifiers}
		switch {
		case a.Ref != "":
			r, err = p.Click(ctx, a.Ref, o)
		case a.X != nil && a.Y != nil:
			r, err = p.ClickXY(ctx, *a.X, *a.Y, o)
		default:
			err = errors.New("click: ref or x,y required")
		}
	case "hover":
		if a.Ref == "" {
			return nil, errors.New("hover: ref required")
		}
		r, err = p.Hover(ctx, a.Ref)
	case "type":
		if a.Ref == "" {
			return nil, errors.New("type: ref required (use keytype for the focused element)")
		}
		r, err = p.Type(ctx, a.Ref, a.Text, page.TypeOptions{Append: a.Append, Submit: a.Submit})
	case "select":
		r, err = p.Select(ctx, a.Ref, a.Values)
	case "key":
		r, err = p.Key(ctx, a.Combo)
	case "keytype":
		r, err = p.KeyType(ctx, a.Text)
	case "scroll":
		if a.DX == 0 && a.DY == 0 {
			a.DY = 600
		}
		if a.X != nil && a.Y != nil {
			r, err = p.ScrollXY(ctx, *a.X, *a.Y, a.DX, a.DY)
		} else {
			r, err = p.Scroll(ctx, a.Ref, a.DX, a.DY)
		}
	case "drag":
		switch {
		case a.From != "" && a.To != "":
			r, err = p.Drag(ctx, a.From, a.To)
		case a.X != nil && a.Y != nil && a.X2 != nil && a.Y2 != nil:
			r, err = p.DragXY(ctx, *a.X, *a.Y, *a.X2, *a.Y2)
		default:
			err = errors.New("drag: from+to refs or x,y,x2,y2 required")
		}
	case "upload":
		r, err = p.Upload(ctx, a.Ref, a.Files)
	default:
		return nil, fmt.Errorf("unknown tool %q", method)
	}
	if err != nil {
		return nil, err
	}
	return s.reply(ctx, b, p, r, before, a)
}

// reply adds tabs opened by the action and, if asked, a fresh snapshot.
func (s *Server) reply(ctx context.Context, b *browser.Browser, p *page.Page, r *page.Result, before []*target.Info, a Params) (*ActionReply, error) {
	out := &ActionReply{Result: r}
	if before != nil {
		seen := map[target.ID]bool{}
		for _, t := range before {
			seen[t.TargetID] = true
		}
		if after, err := b.Pages(ctx); err == nil {
			for _, t := range after {
				if !seen[t.TargetID] {
					out.NewTabs = append(out.NewTabs, TabInfo{TargetID: string(t.TargetID), URL: t.URL, Title: t.Title})
				}
			}
		}
		if len(out.NewTabs) > 0 && r.Note == "" {
			r.Note = "a new tab opened; use tab_switch to drive it"
		}
	}
	if a.Snapshot && r.Dialog == nil {
		if snap, err := p.Snapshot(ctx, page.SnapshotOptions{MaxElements: a.MaxElements, MaxText: a.MaxText}); err == nil {
			out.Snapshot = snap.String()
		}
	}
	return out, nil
}

func orNull(v []byte) []byte {
	if len(v) == 0 {
		return []byte("null")
	}
	return v
}

// agentEnv lets the Jev loop use (and follow) the daemon's tabs.
type agentEnv struct {
	s *Server
	b *browser.Browser
}

func (e agentEnv) Current(ctx context.Context) (*page.Page, error) { return e.s.current(ctx, e.b) }
func (e agentEnv) Switch(ctx context.Context, id string) (*page.Page, error) {
	return e.s.use(ctx, e.b, target.ID(id))
}
func (e agentEnv) TabIDs(ctx context.Context) ([]string, error) {
	ps, err := e.b.Pages(ctx)
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, string(p.TargetID))
	}
	return ids, err
}

func (s *Server) run(ctx context.Context, b *browser.Browser, raw json.RawMessage) (any, error) {
	var t agent.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("run: bad params: %w", err)
	}
	s.mu.Lock()
	LoadEnvFile()
	// Rebuilt when the provider or key changes (forwarded by each MCP server).
	if key, endpoint, _ := jev.KeyFromEnv(); s.jev == nil || s.jev.Key != key || s.jev.Endpoint != endpoint {
		s.llm = nil
		j, err := jev.FromEnv()
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.jev = j
		if l, err := llm.FromEnv(); err == nil {
			s.llm = l
		}
	}
	j, l := s.jev, s.llm
	s.mu.Unlock()
	r := &agent.Runner{Jev: j, Env: agentEnv{s, b}}
	if l != nil {
		r.LLM = l
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return r.Run(ctx, t)
}

// LoadEnvFile reads KEY=VALUE lines from $BROJEV_HOME/env into the process
// environment without overriding variables that are already set.
func LoadEnvFile() {
	b, err := os.ReadFile(filepath.Join(Home(), "env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(k, "#") || os.Getenv(k) != "" {
			continue
		}
		os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
	}
}
