package page

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"

	"github.com/barockok/brojev/internal/keyboard"
)

// Result describes what an action did, so the caller rarely needs another
// round trip to find out.
type Result struct {
	Navigated bool    `json:"navigated,omitempty"`
	URL       string  `json:"url"`
	Title     string  `json:"title"`
	Dialog    *Dialog `json:"dialog,omitempty"`
	Note      string  `json:"note,omitempty"`
	MS        int64   `json:"ms"`
}

func (p *Page) keyboard() *keyboard.Keyboard {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.kb == nil {
		p.kb = keyboard.New(p.S)
	}
	return p.kb
}

// do runs an input action, returning early if it opens a JS dialog (the
// protocol call then stays blocked until the dialog is handled), and waits
// briefly for any navigation it started.
func (p *Page) do(ctx context.Context, fn func(ctx context.Context) error) (*Result, error) {
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	start := time.Now()
	select { // drop a stale signal
	case <-p.dialogs:
	default:
	}
	nav := p.watchNav()
	defer nav.cancel()

	actx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	done := make(chan error, 1)
	go func() {
		defer cancel()
		done <- fn(actx)
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
	case <-p.dialogs:
		r := &Result{Dialog: p.OpenDialog(), MS: time.Since(start).Milliseconds()}
		r.Note = "the action opened a dialog; handle it with dialog accept/dismiss"
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r := &Result{}
	if d := nav.wait(ctx, p.mainFrameID(), p.dialogs); d {
		r.Navigated = true
	}
	if dlg := p.OpenDialog(); dlg != nil {
		r.Dialog = dlg
	} else if info, err := p.info(ctx); err == nil {
		r.URL, r.Title = info.URL, info.Title
	}
	r.MS = time.Since(start).Milliseconds()
	return r, nil
}

type navWatch struct {
	started <-chan jsonv2.Value
	loaded  <-chan jsonv2.Value // DOMContentLoaded
	stopped <-chan jsonv2.Value // any frame stopped loading
	inDoc   <-chan jsonv2.Value // fragment / history.pushState
	cancel  func()
}

func (p *Page) watchNav() *navWatch {
	started, c1 := p.S.Subscribe("Page.frameStartedLoading")
	loaded, c2 := p.S.Subscribe("Page.domContentEventFired")
	stopped, c3 := p.S.Subscribe("Page.frameStoppedLoading")
	inDoc, c4 := p.S.Subscribe("Page.navigatedWithinDocument")
	return &navWatch{started: started, loaded: loaded, stopped: stopped, inDoc: inDoc, cancel: func() { c1(); c2(); c3(); c4() }}
}

func isFrame(v jsonv2.Value, id cdproto.FrameID) bool {
	var ev struct {
		FrameID cdproto.FrameID `json:"frameId"`
	}
	return jsonv2.Unmarshal(v, &ev) == nil && (id == "" || ev.FrameID == id)
}

// wait reports whether the main frame started loading shortly after the
// action, and if so waits for DOMContentLoaded.
func (w *navWatch) wait(ctx context.Context, main cdproto.FrameID, dialogs <-chan struct{}) bool {
	grace := time.NewTimer(150 * time.Millisecond)
	defer grace.Stop()
	for {
		select {
		case v := <-w.started:
			var ev cdppage.EventFrameStartedLoading
			if jsonv2.Unmarshal(v, &ev) != nil || (main != "" && ev.FrameID != main) {
				continue
			}
			// DOMContentLoaded covers normal loads; frameStoppedLoading also
			// covers back-forward cache restores and same-document jumps,
			// which never fire DOMContentLoaded.
			t := time.NewTimer(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-w.loaded:
					return true
				case v := <-w.stopped:
					if isFrame(v, main) {
						return true
					}
				case v := <-w.inDoc:
					if isFrame(v, main) {
						return true
					}
				case <-t.C:
					return true
				case <-dialogs:
					return true
				case <-ctx.Done():
					return true
				}
			}
		case <-grace.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (p *Page) mainFrameID() cdproto.FrameID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frameByN[0]
}

func (p *Page) info(ctx context.Context) (*struct{ URL, Title string }, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, err
	}
	var v struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := p.eval(ctx, fs[0], "({url: location.href, title: document.title})", &v); err != nil {
		return nil, err
	}
	return &struct{ URL, Title string }{v.URL, v.Title}, nil
}

// locate finds the frame holding ref and that frame's origin in top-level
// coordinates, scrolling ancestor iframes into view first.
func (p *Page) locate(ctx context.Context, r string) (*frame, int, box, error) {
	num, id, err := parseRef(r)
	if err != nil {
		return nil, 0, box{}, err
	}
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, 0, box{}, err
	}
	var f *frame
	byID := map[cdproto.FrameID]*frame{}
	for _, x := range fs {
		byID[x.ID] = x
		if x.num == num {
			f = x
		}
	}
	if f == nil {
		return nil, 0, box{}, fmt.Errorf("stale ref %s: frame f%d is gone, take a new snapshot", r, num)
	}
	if num == 0 {
		return f, id, box{}, nil
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
	origin, _ := p.offsets(ctx, fs)
	o, ok := origin[f.ID]
	if !ok {
		return nil, 0, box{}, fmt.Errorf("frame f%d is not visible", num)
	}
	return f, id, o, nil
}

func (p *Page) revealOwner(ctx context.Context, f, parent *frame) error {
	owner, err := cdproto.Call(ctx, parent.sess, dom.GetFrameOwner, dom.GetFrameOwnerParams{FrameID: f.ID})
	if err != nil {
		return err
	}
	cid, err := p.world(ctx, parent)
	if err != nil {
		return err
	}
	obj, err := cdproto.Call(ctx, parent.sess, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: owner.BackendNodeID, ExecutionContextID: cid})
	if err != nil {
		return err
	}
	_, err = cdproto.Call(ctx, parent.sess, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
		FunctionDeclaration: `function(){const r=this.getBoundingClientRect();
if(r.top<0||r.left<0||r.bottom>innerHeight||r.right>innerWidth)this.scrollIntoView({block:"center",inline:"center",behavior:"instant"})}`,
		ObjectID: obj.Object.ObjectID,
	})
	return err
}

type point struct {
	X, Y      float64
	Tag, Type string
}

// point readies ref for pointer input and returns a top-level point that
// hit-tests to it.
func (p *Page) point(ctx context.Context, r string, opts string) (*frame, int, point, error) {
	f, id, o, err := p.locate(ctx, r)
	if err != nil {
		return nil, 0, point{}, err
	}
	var res struct {
		OK     bool    `json:"ok"`
		Reason string  `json:"reason"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Tag    string  `json:"tag"`
		Type   string  `json:"type"`
	}
	if err := p.eval(ctx, f, fmt.Sprintf("__bj.prep(%d, %s)", id, opts), &res); err != nil {
		return nil, 0, point{}, fmt.Errorf("%s: %w", r, err)
	}
	if !res.OK {
		return nil, 0, point{}, fmt.Errorf("%s: %s", r, res.Reason)
	}
	return f, id, point{X: res.X + o.X, Y: res.Y + o.Y, Tag: res.Tag, Type: res.Type}, nil
}

func (p *Page) mouse(ctx context.Context, typ input.DispatchMouseEventType, x, y float64, btn input.MouseButton, buttons, count int64) error {
	_, err := cdproto.Call(ctx, p.S, input.DispatchMouseEvent, input.DispatchMouseEventParams{
		Type: typ, X: x, Y: y, Button: btn, Buttons: buttons, ClickCount: count, Modifiers: p.keyboard().Modifiers(),
	})
	return err
}

type ClickOptions struct {
	Button    string   `json:"button,omitempty"`    // left (default), right, middle
	Count     int      `json:"count,omitempty"`     // 2 for double click
	Modifiers []string `json:"modifiers,omitempty"` // e.g. ["Shift"], ["ControlOrMeta"]
}

func buttonOf(name string) (input.MouseButton, int64, error) {
	switch strings.ToLower(name) {
	case "", "left":
		return input.MouseButtonLeft, 1, nil
	case "right":
		return input.MouseButtonRight, 2, nil
	case "middle":
		return input.MouseButtonMiddle, 4, nil
	}
	return "", 0, fmt.Errorf("unknown button %q", name)
}

func (p *Page) clickAt(ctx context.Context, x, y float64, o ClickOptions) error {
	btn, bits, err := buttonOf(o.Button)
	if err != nil {
		return err
	}
	kb := p.keyboard()
	for _, m := range o.Modifiers {
		if err := kb.Down(ctx, m); err != nil {
			return err
		}
	}
	defer func() {
		for i := len(o.Modifiers) - 1; i >= 0; i-- {
			_ = kb.Up(context.Background(), o.Modifiers[i])
		}
	}()
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, x, y, input.MouseButtonNone, 0, 0); err != nil {
		return err
	}
	count := max(o.Count, 1)
	for i := 1; i <= count; i++ {
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMousePressed, x, y, btn, bits, int64(i)); err != nil {
			return err
		}
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseReleased, x, y, btn, 0, int64(i)); err != nil {
			return err
		}
	}
	return nil
}

// Click clicks ref like a user: scroll into view, wait until stable, check
// it receives the event, then press and release the mouse.
func (p *Page) Click(ctx context.Context, r string, o ClickOptions) (*Result, error) {
	_, _, pt, err := p.point(ctx, r, "{}")
	if err != nil {
		return nil, err
	}
	return p.do(ctx, func(ctx context.Context) error { return p.clickAt(ctx, pt.X, pt.Y, o) })
}

// ClickXY clicks top-level viewport coordinates (canvas, maps, closed shadow).
func (p *Page) ClickXY(ctx context.Context, x, y float64, o ClickOptions) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error { return p.clickAt(ctx, x, y, o) })
}

func (p *Page) Hover(ctx context.Context, r string) (*Result, error) {
	_, _, pt, err := p.point(ctx, r, "{enabled:false}")
	if err != nil {
		return nil, err
	}
	return p.do(ctx, func(ctx context.Context) error {
		return p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, pt.X, pt.Y, input.MouseButtonNone, 0, 0)
	})
}

var nativePicker = map[string]bool{"date": true, "time": true, "datetime-local": true, "month": true, "week": true, "color": true, "range": true}

type TypeOptions struct {
	Append bool `json:"append,omitempty"` // keep existing content (default replaces it)
	Submit bool `json:"submit,omitempty"` // press Enter afterwards
}

// Type focuses ref and enters text, replacing its content unless Append.
func (p *Page) Type(ctx context.Context, r, text string, o TypeOptions) (*Result, error) {
	f, id, pt, err := p.point(ctx, r, "{editable:true}")
	if err != nil {
		return nil, err
	}
	if pt.Tag == "input" && nativePicker[pt.Type] {
		// Browser-native picker UI: set the value directly (e.g. 2026-11-20).
		var got string
		if err := p.eval(ctx, f, fmt.Sprintf("__bj.setValue(%d, %s)", id, jsString(text)), &got); err != nil {
			return nil, err
		}
		if got != text {
			return nil, fmt.Errorf("%s rejected value %q (format for %s inputs, e.g. date 2026-11-20, time 13:45)", r, text, pt.Type)
		}
		return p.do(ctx, func(context.Context) error { return nil })
	}
	return p.do(ctx, func(ctx context.Context) error {
		if err := p.clickAt(ctx, pt.X, pt.Y, ClickOptions{}); err != nil {
			return err
		}
		var focused bool
		if err := p.eval(ctx, f, fmt.Sprintf("__bj.focusEl(%d, %v)", id, !o.Append), &focused); err != nil {
			return err
		}
		if !focused {
			return fmt.Errorf("%s did not take focus", r)
		}
		kb := p.keyboard()
		if text == "" && !o.Append {
			if err := kb.Press(ctx, "Backspace"); err != nil {
				return err
			}
		} else if text != "" {
			if err := kb.InsertText(ctx, text); err != nil {
				return err
			}
		}
		if o.Submit {
			return kb.Press(ctx, "Enter")
		}
		return nil
	})
}

// Select chooses options of a native <select> by value or label.
func (p *Page) Select(ctx context.Context, r string, values []string) (*Result, error) {
	f, id, _, err := p.locate(ctx, r)
	if err != nil {
		return nil, err
	}
	b, _ := jsonv2.Marshal(values)
	var chosen []string
	if err := p.eval(ctx, f, fmt.Sprintf("__bj.selectOptions(%d, %s)", id, b), &chosen); err != nil {
		return nil, fmt.Errorf("%s: %w", r, err)
	}
	res, err := p.do(ctx, func(context.Context) error { return nil })
	if res != nil {
		res.Note = "selected: " + strings.Join(chosen, ", ")
	}
	return res, err
}

// Key presses a key combo on the focused element, e.g. "Enter", "Meta+A".
func (p *Page) Key(ctx context.Context, combo string) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error { return p.keyboard().Press(ctx, combo) })
}

// KeyType types text into the focused element key by key.
func (p *Page) KeyType(ctx context.Context, text string) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error { return p.keyboard().Type(ctx, text) })
}

// Scroll wheels by (dx, dy) over ref, or over the viewport center if ref is "".
func (p *Page) Scroll(ctx context.Context, r string, dx, dy float64) (*Result, error) {
	var x, y float64
	if r != "" {
		_, _, pt, err := p.point(ctx, r, "{enabled:false}")
		if err != nil {
			// A covered scroller still scrolls under the wheel; fall back to its box.
			return nil, err
		}
		x, y = pt.X, pt.Y
	} else {
		info, err := p.viewport(ctx)
		if err != nil {
			return nil, err
		}
		x, y = info[0]/2, info[1]/2
	}
	return p.ScrollXY(ctx, x, y, dx, dy)
}

// scrollWatchJS(x, y, dx, dy) counts scroll events (capture on window sees
// element scrolls too) and reports whether anything under the point can still
// move in that direction. Settling is polled from Go with short evaluations:
// on headless Linux Chrome a pending awaitPromise evaluation holds back the
// wheel scroll itself, and a software-rendered Chrome can take over a second
// to apply it.
const scrollWatchJS = `((x, y, dx, dy) => {
  globalThis.__brojevScroll = {n: 0, last: performance.now()};
  if (!globalThis.__brojevScrollOn) {
    globalThis.__brojevScrollOn = true;
    addEventListener('scroll', () => { const s = globalThis.__brojevScroll; s.n++; s.last = performance.now(); }, {capture: true, passive: true});
  }
  const can = el => {
    const st = getComputedStyle(el), root = el === document.scrollingElement;
    const ok = v => root ? v !== 'hidden' && v !== 'clip' : v === 'auto' || v === 'scroll' || v === 'overlay';
    const max = [el.scrollWidth - el.clientWidth, el.scrollHeight - el.clientHeight];
    return (dy && ok(st.overflowY) && (dy < 0 ? el.scrollTop > 0 : el.scrollTop < max[1] - 1)) ||
           (dx && ok(st.overflowX) && (dx < 0 ? el.scrollLeft > 0 : el.scrollLeft < max[0] - 1));
  };
  for (let el = document.elementFromPoint(x, y); el; el = el.parentElement)
    if (can(el)) return true;
  return !!document.scrollingElement && can(document.scrollingElement);
})`

func (p *Page) ScrollXY(ctx context.Context, x, y, dx, dy float64) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error {
		var top *frame
		canScroll := true
		if fs, err := p.frames(ctx); err == nil && len(fs) > 0 {
			expr := fmt.Sprintf("%s(%g, %g, %g, %g)", scrollWatchJS, x, y, dx, dy)
			if p.eval(ctx, fs[0], expr, &canScroll) == nil {
				top = fs[0]
			}
		}
		mods := p.keyboard().Modifiers()
		// Move the pointer over the target first, as a real wheel would be.
		if err := p.S.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y, "modifiers": mods,
		}, nil); err != nil {
			return err
		}
		// Raw params: cdproto omits zero deltas, but Chrome requires both.
		if err := p.S.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy, "modifiers": mods,
		}, nil); err != nil {
			return err
		}
		if top == nil {
			time.Sleep(60 * time.Millisecond)
			return nil
		}
		if !canScroll {
			return nil // already at the limit (or the point is inside an iframe)
		}
		// Wait for the first scroll event, then until events stop for 100 ms.
		start := time.Now()
		for time.Since(start) < 2500*time.Millisecond {
			time.Sleep(30 * time.Millisecond)
			var st struct {
				N     int     `json:"n"`
				Quiet float64 `json:"quiet"`
			}
			if p.eval(ctx, top, "({n: globalThis.__brojevScroll.n, quiet: performance.now() - globalThis.__brojevScroll.last})", &st) != nil {
				return nil
			}
			if st.N > 0 && st.Quiet > 100 {
				return nil
			}
		}
		return nil
	})
}

func (p *Page) viewport(ctx context.Context) ([2]float64, error) {
	fs, err := p.frames(ctx)
	if err != nil {
		return [2]float64{}, err
	}
	var v [2]float64
	err = p.eval(ctx, fs[0], "[innerWidth, innerHeight]", &v)
	return v, err
}

// Drag drags from one ref to another. HTML5 drag-and-drop is intercepted and
// replayed through Input.dispatchDragEvent; pointer-based drag libraries get
// plain mouse moves.
func (p *Page) Drag(ctx context.Context, from, to string) (*Result, error) {
	_, _, a, err := p.point(ctx, from, "{enabled:false}")
	if err != nil {
		return nil, err
	}
	_, _, b, err := p.point(ctx, to, "{enabled:false}")
	if err != nil {
		return nil, err
	}
	return p.DragXY(ctx, a.X, a.Y, b.X, b.Y)
}

func (p *Page) DragXY(ctx context.Context, x1, y1, x2, y2 float64) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error {
		if _, err := cdproto.Call(ctx, p.S, input.SetInterceptDrags, input.SetInterceptDragsParams{Enabled: true}); err != nil {
			return err
		}
		defer cdproto.Call(context.Background(), p.S, input.SetInterceptDrags, input.SetInterceptDragsParams{Enabled: false})
		intercepted, cancel := p.S.Subscribe("Input.dragIntercepted")
		defer cancel()

		left := input.MouseButtonLeft
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, x1, y1, input.MouseButtonNone, 0, 0); err != nil {
			return err
		}
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMousePressed, x1, y1, left, 1, 1); err != nil {
			return err
		}
		var data *input.DragData
		const steps = 8
		for i := 1; i <= steps; i++ {
			x := x1 + (x2-x1)*float64(i)/steps
			y := y1 + (y2-y1)*float64(i)/steps
			if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, x, y, left, 1, 0); err != nil {
				return err
			}
			select {
			case v := <-intercepted:
				var ev input.EventDragIntercepted
				if jsonv2.Unmarshal(v, &ev) == nil {
					data = ev.Data
				}
			default:
			}
			if data != nil {
				break
			}
		}
		if data == nil {
			select { // the last move may still be in flight
			case v := <-intercepted:
				var ev input.EventDragIntercepted
				if jsonv2.Unmarshal(v, &ev) == nil {
					data = ev.Data
				}
			case <-time.After(30 * time.Millisecond):
			}
		}
		if data != nil {
			mods := p.keyboard().Modifiers()
			for _, t := range []input.DispatchDragEventType{input.DispatchDragEventTypeDragEnter, input.DispatchDragEventTypeDragOver, input.DispatchDragEventTypeDrop} {
				if _, err := cdproto.Call(ctx, p.S, input.DispatchDragEvent, input.DispatchDragEventParams{Type: t, X: x2, Y: y2, Data: data, Modifiers: mods}); err != nil {
					return err
				}
			}
		}
		return p.mouse(ctx, input.DispatchMouseEventTypeMouseReleased, x2, y2, left, 0, 1)
	})
}

// Upload sets files on ref: a file input directly, or any control that opens
// a file chooser (the chooser is intercepted, never shown).
func (p *Page) Upload(ctx context.Context, r string, files []string) (*Result, error) {
	abs := make([]string, len(files))
	for i, f := range files {
		a, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(a); err != nil {
			return nil, fmt.Errorf("upload: %w", err)
		}
		abs[i] = a
	}
	f, id, _, err := p.locate(ctx, r)
	if err != nil {
		return nil, err
	}
	obj, err := p.objectOf(ctx, f, id)
	if err != nil {
		return nil, err
	}
	node, err := cdproto.Call(ctx, f.sess, dom.DescribeNode, dom.DescribeNodeParams{ObjectID: obj})
	if err != nil {
		return nil, err
	}
	isFile := node.Node != nil && strings.EqualFold(node.Node.NodeName, "input") && attr(node.Node.Attributes, "type") == "file"
	if isFile {
		if _, err := cdproto.Call(ctx, f.sess, dom.SetFileInputFiles, dom.SetFileInputFilesParams{Files: abs, BackendNodeID: node.Node.BackendNodeID}); err != nil {
			return nil, err
		}
		res, err := p.do(ctx, func(context.Context) error { return nil })
		if res != nil {
			res.Note = fmt.Sprintf("set %d file(s)", len(abs))
		}
		return res, err
	}

	// Not a file input: click it with the chooser intercepted.
	if _, err := cdproto.Call(ctx, p.S, cdppage.SetInterceptFileChooserDialog, cdppage.SetInterceptFileChooserDialogParams{Enabled: true}); err != nil {
		return nil, err
	}
	defer cdproto.Call(context.Background(), p.S, cdppage.SetInterceptFileChooserDialog, cdppage.SetInterceptFileChooserDialogParams{Enabled: false})
	chooser, cancel := p.S.Subscribe("Page.fileChooserOpened")
	defer cancel()
	if _, err := p.Click(ctx, r, ClickOptions{}); err != nil {
		return nil, err
	}
	select {
	case v := <-chooser:
		var ev cdppage.EventFileChooserOpened
		if err := jsonv2.Unmarshal(v, &ev); err != nil {
			return nil, err
		}
		if ev.Mode == cdppage.FileChooserOpenedModeSelectSingle && len(abs) > 1 {
			return nil, errors.New("upload: this chooser accepts a single file")
		}
		if _, err := cdproto.Call(ctx, p.S, dom.SetFileInputFiles, dom.SetFileInputFilesParams{Files: abs, BackendNodeID: ev.BackendNodeID}); err != nil {
			return nil, err
		}
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("upload: clicking %s did not open a file chooser", r)
	}
	res, err := p.do(ctx, func(context.Context) error { return nil })
	if res != nil {
		res.Note = fmt.Sprintf("set %d file(s) via file chooser", len(abs))
	}
	return res, err
}

func attr(attrs []string, name string) string {
	for i := 0; i+1 < len(attrs); i += 2 {
		if strings.EqualFold(attrs[i], name) {
			return strings.ToLower(attrs[i+1])
		}
	}
	return ""
}

// Front makes this the active tab of its window.
func (p *Page) Front(ctx context.Context) error {
	_, err := cdproto.Call(ctx, p.S, cdppage.BringToFront, cdproto.Empty{})
	return err
}

// Navigate loads url in this tab.
func (p *Page) Navigate(ctx context.Context, url string) (*Result, error) {
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	start := time.Now()
	loaded, cancel := p.S.Subscribe("Page.domContentEventFired")
	defer cancel()
	nav, err := cdproto.Call(ctx, p.S, cdppage.Navigate, cdppage.NavigateParams{URL: url})
	if err != nil {
		return nil, err
	}
	if nav.ErrorText != "" {
		return nil, fmt.Errorf("navigate %s: %s", url, nav.ErrorText)
	}
	if nav.LoaderID != "" {
		select {
		case <-loaded:
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("navigate %s: DOMContentLoaded timeout", url)
		case <-p.dialogs:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r := &Result{Navigated: true, Dialog: p.OpenDialog()}
	if r.Dialog == nil {
		if info, err := p.info(ctx); err == nil {
			r.URL, r.Title = info.URL, info.Title
		}
	}
	r.MS = time.Since(start).Milliseconds()
	return r, nil
}

// History moves back (delta -1) or forward (+1).
func (p *Page) History(ctx context.Context, delta int) (*Result, error) {
	h, err := cdproto.Call(ctx, p.S, cdppage.GetNavigationHistory, cdproto.Empty{})
	if err != nil {
		return nil, err
	}
	i := int(h.CurrentIndex) + delta
	if i < 0 || i >= len(h.Entries) {
		return nil, errors.New("no history entry in that direction")
	}
	id := h.Entries[i].ID
	return p.do(ctx, func(ctx context.Context) error {
		_, err := cdproto.Call(ctx, p.S, cdppage.NavigateToHistoryEntry, cdppage.NavigateToHistoryEntryParams{EntryID: id})
		return err
	})
}

func (p *Page) Reload(ctx context.Context) (*Result, error) {
	return p.do(ctx, func(ctx context.Context) error {
		_, err := cdproto.Call(ctx, p.S, cdppage.Reload, cdppage.ReloadParams{})
		return err
	})
}

// Screenshot returns a JPEG of the viewport (or full page).
func (p *Page) Screenshot(ctx context.Context, full bool) ([]byte, error) {
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	q := int64(75)
	res, err := cdproto.Call(ctx, p.S, cdppage.CaptureScreenshot, cdppage.CaptureScreenshotParams{
		Format: cdppage.CaptureScreenshotFormatJpeg, Quality: &q, CaptureBeyondViewport: &full,
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// ScreenshotFit captures the viewport as a JPEG scaled to fit an image model:
// each side within [280, 1568] px and at most 2,400,000 pixels (larger images
// are rejected, not resized, by the API).
func (p *Page) ScreenshotFit(ctx context.Context) ([]byte, error) {
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	vp, err := p.viewport(ctx)
	if err != nil {
		return nil, err
	}
	w, h := vp[0], vp[1]
	if w < 1 || h < 1 {
		return nil, errors.New("empty viewport")
	}
	scale := 1.0
	if m := max(w, h); m > 1568 {
		scale = 1568 / m
	}
	if px := w * h * scale * scale; px > 2400000 {
		scale *= math.Sqrt(2400000 / px)
	}
	if min(w, h)*scale < 280 {
		return nil, fmt.Errorf("viewport %gx%g too small for an image", w, h)
	}
	q := int64(75)
	res, err := cdproto.Call(ctx, p.S, cdppage.CaptureScreenshot, cdppage.CaptureScreenshotParams{
		Format: cdppage.CaptureScreenshotFormatJpeg, Quality: &q,
		Clip: &cdppage.Viewport{X: 0, Y: 0, Width: w, Height: h, Scale: scale},
	})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// Eval runs JavaScript in the page's main world (page globals visible).
func (p *Page) Eval(ctx context.Context, js string) (jsonv2.Value, error) {
	if p.OpenDialog() != nil {
		return nil, ErrDialogOpen
	}
	yes := true
	res, err := cdproto.Call(ctx, p.S, runtime.Evaluate, runtime.EvaluateParams{Expression: js, ReturnByValue: &yes, AwaitPromise: &yes, UserGesture: &yes})
	if err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, jsError(res.ExceptionDetails)
	}
	if res.Result == nil {
		return nil, nil
	}
	return res.Result.Value, nil
}

// WaitText waits until text appears (or, with gone, disappears) in any frame.
func (p *Page) WaitText(ctx context.Context, text string, gone bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if p.OpenDialog() != nil {
			return ErrDialogOpen
		}
		fs, err := p.frames(ctx)
		if err == nil {
			found := false
			for _, f := range fs {
				var ok bool
				if p.eval(ctx, f, "__bj.hasText("+jsString(text)+")", &ok) == nil && ok {
					found = true
					break
				}
			}
			if found != gone {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if gone {
				return fmt.Errorf("text %q still present after %s", text, timeout)
			}
			return fmt.Errorf("text %q not found after %s", text, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func jsString(s string) string {
	b, _ := jsonv2.Marshal(s)
	return string(b)
}
