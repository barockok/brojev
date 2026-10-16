package page

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	cdproto "github.com/chromedp/cdproto/cdp"
)

// Element is one actionable element. Rect is in top-level viewport CSS px.
type Element struct {
	Ref     string     `json:"ref"`
	Role    string     `json:"role"`
	Name    string     `json:"name,omitempty"`
	Tag     string     `json:"tag"`
	Type    string     `json:"type,omitempty"`
	Value   *string    `json:"value,omitempty"`
	States  []string   `json:"states,omitempty"`
	Href    string     `json:"href,omitempty"`
	Options []string   `json:"options,omitempty"`
	Rect    [4]float64 `json:"rect"`
	InView  bool       `json:"inView"`
	Frame   int        `json:"frame"`
	Covered bool       `json:"covered,omitempty"` // something else is on top of its centre
	Free    string     `json:"free,omitempty"`    // canvas: 5x6 grid, "1" where a click reaches the canvas
}

type Snapshot struct {
	URL      string     `json:"url"`
	Title    string     `json:"title"`
	Viewport [2]float64 `json:"viewport"`
	Scroll   [4]float64 `json:"scroll"`
	Elements []Element  `json:"elements"`
	Text     string     `json:"text"`
	Frames   []string   `json:"frames,omitempty"` // "f1 https://..." for non-main frames
	Dialog   *Dialog    `json:"dialog,omitempty"`
}

type rawSnap struct {
	URL      string     `json:"url"`
	Title    string     `json:"title"`
	Viewport [2]float64 `json:"viewport"`
	Scroll   [4]float64 `json:"scroll"`
	Elements []struct {
		ID      int        `json:"id"`
		Role    string     `json:"role"`
		Name    string     `json:"name"`
		Tag     string     `json:"tag"`
		Type    string     `json:"type"`
		Value   *string    `json:"value"`
		States  []string   `json:"states"`
		Href    string     `json:"href"`
		Options []string   `json:"options"`
		Rect    [4]float64 `json:"rect"`
		InView  bool       `json:"inView"`
		Free    string     `json:"free"`
		Covered bool       `json:"covered"`
	} `json:"elements"`
	Text string `json:"text"`
}

type SnapshotOptions struct {
	MaxElements  int `json:"maxElements,omitempty"`
	MaxOffscreen int `json:"maxOffscreen,omitempty"` // default 150; -1 for none
	MaxText      int `json:"maxText,omitempty"`      // -1 disables text
}

// Snapshot captures all frames in parallel and merges them.
func (p *Page) Snapshot(ctx context.Context, o SnapshotOptions) (*Snapshot, error) {
	if d := p.OpenDialog(); d != nil {
		return &Snapshot{Dialog: d}, nil
	}
	fs, err := p.frames(ctx)
	if err != nil {
		return nil, err
	}
	maxText := o.MaxText
	if maxText == 0 {
		maxText = 6000
	}
	if maxText < 0 {
		maxText = 0
	}
	maxOff := o.MaxOffscreen
	if maxOff == 0 {
		maxOff = 150
	}
	if maxOff < 0 {
		maxOff = 0
	}
	expr := fmt.Sprintf("__bj.snapshot({maxElements:%d,maxText:%d,maxOffscreen:%d})", o.MaxElements, maxText, maxOff)

	raws := make([]*rawSnap, len(fs))
	errs := make([]error, len(fs))
	var wg sync.WaitGroup
	for i, f := range fs {
		wg.Add(1)
		go func(i int, f *frame) {
			defer wg.Done()
			r := new(rawSnap)
			errs[i] = p.eval(ctx, f, expr, r)
			raws[i] = r
		}(i, f)
	}
	var origin, clip map[cdproto.FrameID]box
	if len(fs) > 1 {
		origin, clip = p.offsets(ctx, fs)
	}
	wg.Wait()
	if errs[0] != nil {
		return nil, errs[0]
	}

	main := raws[0]
	s := &Snapshot{URL: main.URL, Title: main.Title, Viewport: main.Viewport, Scroll: main.Scroll, Text: main.Text}
	vw, vh := main.Viewport[0], main.Viewport[1]
	for i, f := range fs {
		r := raws[i]
		if errs[i] != nil || r == nil {
			continue // a frame that navigated or detached mid-snapshot
		}
		var o, c box
		if f.num != 0 {
			var ok bool
			if o, ok = origin[f.ID]; !ok {
				continue // its iframe is not visible
			}
			c = clip[f.ID]
			if c.W <= 0 || c.H <= 0 {
				continue
			}
			host := r.URL
			if u, err := url.Parse(r.URL); err == nil && u.Host != "" {
				host = u.Scheme + "://" + u.Host + u.Path
			}
			s.Frames = append(s.Frames, fmt.Sprintf("f%d %s", f.num, host))
			if r.Text != "" && len(s.Text) < maxText {
				s.Text += fmt.Sprintf(" [f%d] %s", f.num, r.Text)
			}
		}
		for _, e := range r.Elements {
			rect := [4]float64{e.Rect[0] + o.X, e.Rect[1] + o.Y, e.Rect[2], e.Rect[3]}
			inView := e.InView
			if f.num != 0 {
				b := intersect(c, box{rect[0], rect[1], rect[2], rect[3]})
				inView = inView && b.W > 0 && b.H > 0
			}
			inView = inView && rect[0] < vw && rect[1] < vh && rect[0]+rect[2] > 0 && rect[1]+rect[3] > 0
			s.Elements = append(s.Elements, Element{
				Ref: ref(f.num, e.ID), Role: e.Role, Name: e.Name, Tag: e.Tag, Type: e.Type,
				Value: e.Value, States: e.States, Href: e.Href, Options: e.Options,
				Rect: rect, InView: inView, Frame: f.num, Free: e.Free, Covered: e.Covered,
			})
		}
	}
	if len(s.Text) > maxText {
		s.Text = s.Text[:maxText]
	}
	return s, nil
}

func ref(frameNum, id int) string {
	if frameNum == 0 {
		return "e" + strconv.Itoa(id)
	}
	return fmt.Sprintf("f%de%d", frameNum, id)
}

var refRe = regexp.MustCompile(`^(?:f(\d+))?e(\d+)$`)

func parseRef(r string) (frameNum, id int, err error) {
	m := refRe.FindStringSubmatch(strings.TrimSpace(r))
	if m == nil {
		return 0, 0, fmt.Errorf("bad ref %q: want e12 or f2e5", r)
	}
	if m[1] != "" {
		frameNum, _ = strconv.Atoi(m[1])
	}
	id, _ = strconv.Atoi(m[2])
	return frameNum, id, nil
}

// String renders the snapshot compactly for an LLM: in-view elements first,
// then the rest marked by direction, then visible text.
func (s *Snapshot) String() string {
	var b strings.Builder
	if s.Dialog != nil {
		fmt.Fprintf(&b, "DIALOG OPEN (%s): %q\nHandle it with dialog accept/dismiss before anything else.\n", s.Dialog.Type, s.Dialog.Message)
		return b.String()
	}
	fmt.Fprintf(&b, "url: %s\ntitle: %s\n", s.URL, s.Title)
	fmt.Fprintf(&b, "viewport %gx%g, scroll %g/%g of %g\n", s.Viewport[0], s.Viewport[1], s.Scroll[1], s.Scroll[3]-s.Viewport[1], s.Scroll[3])
	for _, f := range s.Frames {
		fmt.Fprintf(&b, "frame %s\n", f)
	}
	var off []Element
	for _, e := range s.Elements {
		if e.InView {
			writeEl(&b, e, "")
		} else {
			off = append(off, e)
		}
	}
	if len(off) > 0 {
		b.WriteString("-- offscreen --\n")
		for _, e := range off {
			dir := "below"
			switch {
			case e.Rect[1]+e.Rect[3] <= 0:
				dir = "above"
			case e.Rect[0] >= s.Viewport[0]:
				dir = "right"
			case e.Rect[0]+e.Rect[2] <= 0:
				dir = "left"
			}
			writeEl(&b, e, dir)
		}
	}
	if s.Text != "" {
		b.WriteString("-- visible text --\n")
		b.WriteString(s.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func writeEl(b *strings.Builder, e Element, dir string) {
	fmt.Fprintf(b, "[%s] %s", e.Ref, e.Role)
	if e.Type != "" && e.Type != e.Role && e.Role != "textbox" && e.Role != "button" {
		fmt.Fprintf(b, "(%s)", e.Type)
	}
	if e.Name != "" {
		fmt.Fprintf(b, " %q", e.Name)
	}
	if e.Value != nil {
		fmt.Fprintf(b, " value=%q", *e.Value)
	}
	for _, st := range e.States {
		b.WriteString(" " + st)
	}
	if len(e.Options) > 0 {
		fmt.Fprintf(b, " options=[%s]", strings.Join(e.Options, " | "))
	}
	if e.Href != "" && e.Role == "link" {
		fmt.Fprintf(b, " -> %s", shortURL(e.Href))
	}
	if dir != "" {
		b.WriteString(" (" + dir + ")")
	}
	b.WriteByte('\n')
}

func shortURL(s string) string {
	if len(s) > 80 {
		return s[:79] + "…"
	}
	return s
}
