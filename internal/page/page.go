// Package page drives one tab: frames (including out-of-process iframes),
// isolated-world snapshots, element refs, and input. No Playwright.
package page

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/cdp"
	"github.com/barockok/brojev/internal/keyboard"
)

//go:embed lib.js
var lib string

const worldName = "brojev"

// Page is one tab plus the sessions of its out-of-process iframes.
type Page struct {
	TargetID target.ID
	S        *cdp.Session // the tab's session; all input goes here

	mu        sync.Mutex
	worlds    map[cdproto.FrameID]runtime.ExecutionContextID
	frameNum  map[cdproto.FrameID]int
	frameByN  map[int]cdproto.FrameID
	nextFrame int
	oopif     map[cdproto.FrameID]*cdp.Session // frame id == iframe target id
	dialog    *Dialog
	kb        *keyboard.Keyboard
	dialogs   chan struct{} // receives when a dialog opens
	cancels   []func()
	ctx       context.Context
	stop      context.CancelFunc
}

// Dialog is an open alert/confirm/prompt/beforeunload. While one is open the
// page's JS is frozen, so other actions fail fast instead of hanging.
type Dialog struct {
	Type          string `json:"type"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"defaultPrompt,omitempty"`
	URL           string `json:"url"`
	sess          *cdp.Session
}

var ErrDialogOpen = errors.New("a JavaScript dialog is open; handle it with dialog(accept|dismiss) first")

// New wraps an attached tab session (Page domain already enabled).
func New(ctx context.Context, id target.ID, s *cdp.Session) (*Page, error) {
	pctx, stop := context.WithCancel(context.Background())
	p := &Page{
		TargetID: id, S: s,
		worlds:   map[cdproto.FrameID]runtime.ExecutionContextID{},
		frameNum: map[cdproto.FrameID]int{},
		frameByN: map[int]cdproto.FrameID{},
		oopif:    map[cdproto.FrameID]*cdp.Session{},
		dialogs:  make(chan struct{}, 1),
		ctx:      pctx, stop: stop,
	}
	p.watchSession(s)
	// Keep rendering and rAF alive when the window is not focused.
	if _, err := cdproto.Call(ctx, s, emulation.SetFocusEmulationEnabled, emulation.SetFocusEmulationEnabledParams{Enabled: true}); err != nil {
		return nil, err
	}
	if err := p.autoAttach(ctx, s); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Page) Close() {
	p.stop()
	p.mu.Lock()
	for _, c := range p.cancels {
		c()
	}
	p.cancels = nil
	p.mu.Unlock()
}

func (p *Page) autoAttach(ctx context.Context, s *cdp.Session) error {
	flat := true
	_, err := cdproto.Call(ctx, s, target.SetAutoAttach, target.SetAutoAttachParams{
		AutoAttach: true, WaitForDebuggerOnStart: false, Flatten: &flat,
	})
	return err
}

// watchSession tracks dialogs, frame navigations and child iframe targets
// for a session (the tab, or an OOPIF).
func (p *Page) watchSession(s *cdp.Session) {
	listen := func(method string, fn func(jsonv2.Value)) {
		ch, cancel := s.Subscribe(method)
		p.mu.Lock()
		p.cancels = append(p.cancels, cancel)
		p.mu.Unlock()
		go func() {
			for v := range ch {
				fn(v)
			}
		}()
	}
	listen("Page.javascriptDialogOpening", func(v jsonv2.Value) {
		var ev cdppage.EventJavascriptDialogOpening
		if jsonv2.Unmarshal(v, &ev) != nil {
			return
		}
		p.mu.Lock()
		p.dialog = &Dialog{Type: string(ev.Type), Message: ev.Message, DefaultPrompt: ev.DefaultPrompt, URL: ev.URL, sess: s}
		p.mu.Unlock()
		select {
		case p.dialogs <- struct{}{}:
		default:
		}
	})
	listen("Page.javascriptDialogClosed", func(jsonv2.Value) {
		p.mu.Lock()
		p.dialog = nil
		p.mu.Unlock()
	})
	listen("Page.frameNavigated", func(v jsonv2.Value) {
		var ev struct {
			Frame struct {
				ID cdproto.FrameID `json:"id"`
			} `json:"frame"`
		}
		if jsonv2.Unmarshal(v, &ev) == nil {
			p.mu.Lock()
			delete(p.worlds, ev.Frame.ID) // new document, new isolated world
			p.mu.Unlock()
		}
	})
	listen("Target.attachedToTarget", func(v jsonv2.Value) {
		var ev target.EventAttachedToTarget
		if jsonv2.Unmarshal(v, &ev) != nil || ev.TargetInfo == nil || ev.TargetInfo.Type != "iframe" {
			return
		}
		child := s.Conn().Session(string(ev.SessionID))
		p.mu.Lock()
		p.oopif[cdproto.FrameID(ev.TargetInfo.TargetID)] = child
		p.mu.Unlock()
		p.watchSession(child)
		go func() {
			_, _ = cdproto.Call(p.ctx, child, cdppage.Enable, cdppage.EnableParams{})
			_ = p.autoAttach(p.ctx, child) // nested OOPIFs
		}()
	})
	listen("Target.detachedFromTarget", func(v jsonv2.Value) {
		var ev struct {
			TargetID cdproto.FrameID `json:"targetId"`
		}
		if jsonv2.Unmarshal(v, &ev) == nil && ev.TargetID != "" {
			p.mu.Lock()
			delete(p.oopif, ev.TargetID)
			delete(p.worlds, ev.TargetID)
			p.mu.Unlock()
		}
	})
}

// OpenDialog returns the open dialog, if any.
func (p *Page) OpenDialog() *Dialog {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialog
}

// HandleDialog accepts or dismisses the open dialog.
func (p *Page) HandleDialog(ctx context.Context, accept bool, text string) (*Dialog, error) {
	d := p.OpenDialog()
	if d == nil {
		return nil, errors.New("no dialog is open")
	}
	_, err := cdproto.Call(ctx, d.sess, cdppage.HandleJavaScriptDialog, cdppage.HandleJavaScriptDialogParams{Accept: accept, PromptText: text})
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.dialog == d {
		p.dialog = nil
	}
	p.mu.Unlock()
	return d, nil
}

// frame is a frame plus the session that can run code in it.
type frame struct {
	ID     cdproto.FrameID
	Parent cdproto.FrameID
	URL    string
	sess   *cdp.Session
	num    int
}

func (p *Page) number(id cdproto.FrameID, main bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if main {
		p.frameNum[id], p.frameByN[0] = 0, id
		return 0
	}
	n, ok := p.frameNum[id]
	if !ok {
		p.nextFrame++
		n = p.nextFrame
		p.frameNum[id], p.frameByN[n] = n, id
	}
	return n
}

// frames lists every frame, main first, each paired with its session.
func (p *Page) frames(ctx context.Context) ([]*frame, error) {
	// An open dialog freezes the renderer; any frame call would hang.
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	root, err := cdproto.Call(ctx, p.S, cdppage.GetFrameTree, cdproto.Empty{})
	if err != nil {
		return nil, err
	}
	var out []*frame
	seen := map[cdproto.FrameID]bool{}
	var walk func(t *cdppage.FrameTree, s *cdp.Session, main bool) error
	walk = func(t *cdppage.FrameTree, s *cdp.Session, main bool) error {
		if t == nil || t.Frame == nil {
			return nil
		}
		id := t.Frame.ID
		p.mu.Lock()
		child := p.oopif[id]
		p.mu.Unlock()
		if child != nil && child != s {
			// This frame lives in another process: its subtree comes from
			// the iframe's own session.
			sub, err := cdproto.Call(ctx, child, cdppage.GetFrameTree, cdproto.Empty{})
			if err != nil {
				return nil // detached mid-walk; skip
			}
			if sub.FrameTree != nil && sub.FrameTree.Frame != nil && sub.FrameTree.Frame.ParentID == "" {
				sub.FrameTree.Frame.ParentID = t.Frame.ParentID
			}
			return walk(sub.FrameTree, child, false)
		}
		if seen[id] {
			return nil
		}
		seen[id] = true
		out = append(out, &frame{ID: id, Parent: t.Frame.ParentID, URL: t.Frame.URL, sess: s, num: p.number(id, main)})
		for _, c := range t.ChildFrames {
			if err := walk(c, s, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.FrameTree, p.S, true); err != nil {
		return nil, err
	}
	// OOPIFs the main tree did not list (Chrome may omit remote frames).
	p.mu.Lock()
	extra := map[cdproto.FrameID]*cdp.Session{}
	for id, s := range p.oopif {
		if !seen[id] {
			extra[id] = s
		}
	}
	p.mu.Unlock()
	for _, s := range extra {
		sub, err := cdproto.Call(ctx, s, cdppage.GetFrameTree, cdproto.Empty{})
		if err == nil {
			_ = walk(sub.FrameTree, s, false)
		}
	}
	return out, nil
}

func (p *Page) world(ctx context.Context, f *frame) (runtime.ExecutionContextID, error) {
	p.mu.Lock()
	id, ok := p.worlds[f.ID]
	p.mu.Unlock()
	if ok {
		return id, nil
	}
	res, err := cdproto.Call(ctx, f.sess, cdppage.CreateIsolatedWorld, cdppage.CreateIsolatedWorldParams{FrameID: f.ID, WorldName: worldName})
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	p.worlds[f.ID] = res.ExecutionContextID
	p.mu.Unlock()
	return res.ExecutionContextID, nil
}

func staleContext(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Cannot find context") || strings.Contains(s, "context was destroyed") ||
		strings.Contains(s, "Cannot find execution context")
}

// eval runs expr (with the brojev lib loaded) in f's isolated world.
func (p *Page) eval(ctx context.Context, f *frame, expr string, out any) error {
	for attempt := 0; ; attempt++ {
		cid, err := p.world(ctx, f)
		if err != nil {
			return err
		}
		yes := true
		res, err := cdproto.Call(ctx, f.sess, runtime.Evaluate, runtime.EvaluateParams{
			Expression: lib + "\n" + expr, ContextID: cid, ReturnByValue: &yes, AwaitPromise: &yes,
		})
		if staleContext(err) && attempt == 0 {
			p.mu.Lock()
			delete(p.worlds, f.ID)
			p.mu.Unlock()
			continue
		}
		if err != nil {
			return err
		}
		if res.ExceptionDetails != nil {
			return jsError(res.ExceptionDetails)
		}
		if out != nil && res.Result != nil && len(res.Result.Value) > 0 {
			return jsonv2.Unmarshal(res.Result.Value, out)
		}
		return nil
	}
}

// objectOf returns a remote object handle for element id in f.
func (p *Page) objectOf(ctx context.Context, f *frame, id int) (runtime.RemoteObjectID, error) {
	for attempt := 0; ; attempt++ {
		cid, err := p.world(ctx, f)
		if err != nil {
			return "", err
		}
		res, err := cdproto.Call(ctx, f.sess, runtime.Evaluate, runtime.EvaluateParams{
			Expression: lib + fmt.Sprintf("\n__bj.get(%d)", id), ContextID: cid,
		})
		if staleContext(err) && attempt == 0 {
			p.mu.Lock()
			delete(p.worlds, f.ID)
			p.mu.Unlock()
			continue
		}
		if err != nil {
			return "", err
		}
		if res.ExceptionDetails != nil {
			return "", jsError(res.ExceptionDetails)
		}
		return res.Result.ObjectID, nil
	}
}

func jsError(d *runtime.ExceptionDetails) error {
	msg := d.Text
	if d.Exception != nil && d.Exception.Description != "" {
		msg = d.Exception.Description
		if i := strings.Index(msg, "\n    at "); i > 0 {
			msg = msg[:i]
		}
	}
	return errors.New(strings.TrimPrefix(msg, "Error: "))
}

type box struct{ X, Y, W, H float64 }

// ownerBox returns the content box of frame f's <iframe> element in its
// parent's viewport coordinates.
func (p *Page) ownerBox(ctx context.Context, f, parent *frame) (box, error) {
	owner, err := cdproto.Call(ctx, parent.sess, dom.GetFrameOwner, dom.GetFrameOwnerParams{FrameID: f.ID})
	if err != nil {
		return box{}, err
	}
	cid, err := p.world(ctx, parent)
	if err != nil {
		return box{}, err
	}
	obj, err := cdproto.Call(ctx, parent.sess, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: owner.BackendNodeID, ExecutionContextID: cid})
	if err != nil {
		return box{}, err
	}
	yes := true
	res, err := cdproto.Call(ctx, parent.sess, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
		FunctionDeclaration: "function(){" + lib + "\nreturn __bj.frameBox(this)}",
		ObjectID:            obj.Object.ObjectID, ReturnByValue: &yes,
	})
	if err != nil {
		return box{}, err
	}
	if res.ExceptionDetails != nil {
		return box{}, jsError(res.ExceptionDetails)
	}
	var b struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if err := jsonv2.Unmarshal(res.Result.Value, &b); err != nil {
		return box{}, err
	}
	return box{b.X, b.Y, b.W, b.H}, nil
}

// offsets returns, per frame, its origin in top-level viewport coordinates
// and its visible clip (intersection of all ancestor iframe boxes).
func (p *Page) offsets(ctx context.Context, fs []*frame) (map[cdproto.FrameID]box, map[cdproto.FrameID]box) {
	byID := map[cdproto.FrameID]*frame{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	local := map[cdproto.FrameID]box{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, f := range fs[1:] {
		parent := byID[f.Parent]
		if parent == nil {
			continue
		}
		wg.Add(1)
		go func(f, parent *frame) {
			defer wg.Done()
			b, err := p.ownerBox(ctx, f, parent)
			if err == nil {
				mu.Lock()
				local[f.ID] = b
				mu.Unlock()
			}
		}(f, parent)
	}
	wg.Wait()
	origin := map[cdproto.FrameID]box{}
	clip := map[cdproto.FrameID]box{}
	var resolve func(id cdproto.FrameID) (box, box, bool)
	resolve = func(id cdproto.FrameID) (box, box, bool) {
		if o, ok := origin[id]; ok {
			return o, clip[id], true
		}
		f := byID[id]
		if f == nil {
			return box{}, box{}, false
		}
		if f.num == 0 {
			o := box{}
			c := box{X: -1e9, Y: -1e9, W: 2e9, H: 2e9}
			origin[id], clip[id] = o, c
			return o, c, true
		}
		b, ok := local[id]
		if !ok {
			return box{}, box{}, false
		}
		po, pc, ok := resolve(f.Parent)
		if !ok {
			return box{}, box{}, false
		}
		o := box{X: po.X + b.X, Y: po.Y + b.Y, W: b.W, H: b.H}
		c := intersect(pc, o)
		origin[id], clip[id] = o, c
		return o, c, true
	}
	for _, f := range fs {
		resolve(f.ID)
	}
	return origin, clip
}

func intersect(a, b box) box {
	x1, y1 := max(a.X, b.X), max(a.Y, b.Y)
	x2, y2 := min(a.X+a.W, b.X+b.W), min(a.Y+a.H, b.Y+b.H)
	if x2 < x1 {
		x2 = x1
	}
	if y2 < y1 {
		y2 = y1
	}
	return box{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}
}
