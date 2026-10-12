package page

// Browser tests against local fixtures. Run with: BROJEV_E2E=1 go test ./internal/page/
// They launch a headless Chrome on a throwaway profile; no network, no models.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
)

var fixtures = map[string]string{
	"/form": `<title>form</title>
<form id=f onsubmit="event.preventDefault(); out.textContent='submitted: '+[...new FormData(f)].map(([k,v])=>k+'='+v).join('&')">
<label>Full name <input name=name></label>
<label for=pw>Password</label><input id=pw name=pw type=password>
<select name=city aria-label="City"><option value=zrh>Zurich</option><option value=lon>London</option></select>
<label><input type=checkbox name=terms value=yes> Accept terms</label>
<label><input type=radio name=size value=s> Small</label><label><input type=radio name=size value=l> Large</label>
<textarea name=note placeholder="Note"></textarea>
<input type=date name=day aria-label="Day">
<div contenteditable=true id=ed aria-label="Editor"></div>
<button>Send</button></form><p id=out></p>`,

	"/shadow": `<title>shadow</title><x-card></x-card><p id=out>0</p>
<script>customElements.define('x-card', class extends HTMLElement { connectedCallback() {
  const r = this.attachShadow({mode:'open'});
  r.innerHTML = '<input aria-label="Shadow input"><button>Shadow button</button>';
  r.querySelector('button').onclick = () => out.textContent = String(+out.textContent + 1);
}});</script>`,

	"/frames": `<title>frames</title><p>parent</p>
<iframe src="/inner" width=300 height=120></iframe>
<iframe id=x width=300 height=160></iframe>
<script>x.src = location.href.replace('127.0.0.1', 'localhost').replace('/frames', '/cross');</script>`,
	"/inner": `<button onclick="this.textContent='inner clicked'">Inner button</button>`,
	"/cross": `<div style="height:40px"></div><input aria-label="Cross input"><button onclick="this.textContent='cross clicked: '+document.querySelector('input').value">Cross button</button>`,

	"/clickable": `<title>clickable</title><div id=d style="cursor:pointer;padding:10px;width:120px"><span>Open panel</span></div><p id=out></p>
<script>d.addEventListener('click', () => out.textContent = 'panel opened');</script>`,

	"/covered": `<title>covered</title><button onclick="out.textContent='clicked'">Hidden target</button><p id=out></p>
<div style="position:fixed;inset:0;background:white;z-index:9" id=overlay>overlay</div>`,

	"/dialogs": `<title>dialogs</title>
<button onclick="alert('hello alert'); out.textContent='after alert'">Alert</button>
<button onclick="out.textContent = confirm('sure?') ? 'confirmed' : 'cancelled'">Confirm</button>
<button onclick="out.textContent = 'prompt: ' + prompt('your name?', 'x')">Prompt</button><p id=out></p>`,

	"/upload": `<title>upload</title><input type=file id=direct aria-label="Direct file" onchange="out.textContent='direct: '+this.files[0].name">
<input type=file id=picker style="display:none" onchange="out.textContent='chooser: '+this.files[0].name">
<button onclick="document.getElementById('picker').click()">Choose file</button><p id=out></p>`,

	"/drag": `<title>drag</title>
<div id=item draggable=true style="width:80px;height:40px;background:#ccc" aria-label="Item" tabindex=0>Item</div>
<div id=zone style="width:200px;height:100px;border:1px solid;margin-top:20px" aria-label="Drop zone" tabindex=0>Drop here</div>
<div id=knob style="position:relative;width:20px;height:20px;background:red;margin-top:20px" aria-label="Knob" tabindex=0></div><p id=out></p>
<script>
item.ondragstart = e => e.dataTransfer.setData('text/plain', 'item');
zone.ondragover = e => e.preventDefault();
zone.ondrop = e => { e.preventDefault(); out.textContent = 'dropped ' + e.dataTransfer.getData('text/plain'); };
let down = null;
knob.onpointerdown = e => { down = e.clientX; knob.setPointerCapture(e.pointerId); };
knob.onpointerup = e => { if (down !== null) out.textContent = 'knob moved ' + Math.round(e.clientX - down); down = null; };
</script>`,

	"/hover": `<title>hover</title><style>#menu{display:none} #wrap:hover #menu{display:block}</style>
<div id=wrap><button>Products</button><div id=menu><a href="#gadgets">Gadgets</a></div></div>`,

	"/keys": `<title>keys</title><input aria-label="A"><input aria-label="B" onfocus="out.textContent='B focused'"><p id=out></p>`,

	"/scroll": `<title>scroll</title><div id=box aria-label="Scroller" tabindex=0 style="height:150px;overflow:auto;border:1px solid">
<div style="height:1200px"></div><button onclick="out.textContent='deep clicked'">Deep button</button></div><p id=out></p>`,

	"/nav":  `<title>nav one</title><a href="/nav2">Next page</a>`,
	"/nav2": `<title>nav two</title><p>arrived</p>`,
}

type env struct {
	t    *testing.T
	base string
	b    *browser.Browser
}

var shared *env

func TestMain(m *testing.M) {
	if os.Getenv("BROJEV_E2E") != "1" {
		fmt.Println("skipping browser tests; set BROJEV_E2E=1")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "brojev-e2e-")
	if err != nil {
		panic(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := fixtures[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><meta charset=utf-8>"+body)
	}))
	ctx := context.Background()
	ep, _, err := chrome.Ensure(ctx, chrome.Options{Profile: filepath.Join(dir, "profile"), Headless: os.Getenv("BROJEV_HEADED") != "1", Log: filepath.Join(dir, "chrome.log")})
	if err != nil {
		panic(err)
	}
	b, err := browser.Connect(ctx, ep)
	if err != nil {
		panic(err)
	}
	shared = &env{base: "http://" + ln.Addr().String(), b: b}
	code := m.Run()
	b.Conn().Browser().Call(ctx, "Browser.close", nil, nil)
	b.Close()
	time.Sleep(300 * time.Millisecond)
	os.RemoveAll(dir)
	os.Exit(code)
}

func open(t *testing.T, path string) (*Page, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	info, err := shared.b.Open(ctx, shared.base+path)
	if err != nil {
		t.Fatal(err)
	}
	tab, err := shared.b.Attach(ctx, target.ID(info.TargetID))
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(ctx, tab.ID, tab.Session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		shared.b.CloseTab(context.Background(), tab.ID)
	})
	return p, ctx
}

func snap(t *testing.T, p *Page, ctx context.Context) *Snapshot {
	t.Helper()
	s, err := p.Snapshot(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// find returns the ref of the element with role and name containing name.
func find(t *testing.T, s *Snapshot, role, name string) string {
	t.Helper()
	for _, e := range s.Elements {
		if (role == "" || e.Role == role) && strings.Contains(e.Name, name) {
			return e.Ref
		}
	}
	t.Fatalf("no %s %q in snapshot:\n%s", role, name, s)
	return ""
}

func text(t *testing.T, p *Page, ctx context.Context, want string) {
	t.Helper()
	if err := p.WaitText(ctx, want, false, 3*time.Second); err != nil {
		s, _ := p.Snapshot(ctx, SnapshotOptions{})
		t.Fatalf("%v\n%s", err, s)
	}
}

// need fails the test on error: need[*Result](t)(p.Click(...)).
func need[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestForm(t *testing.T) {
	p, ctx := open(t, "/form")
	s := snap(t, p, ctx)
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Full name"), "Ada Lovelace", TypeOptions{}))
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Password"), "s3cret", TypeOptions{}))
	need[*Result](t)(p.Select(ctx, find(t, s, "combobox", "City"), []string{"London"}))
	need[*Result](t)(p.Click(ctx, find(t, s, "checkbox", "Accept terms"), ClickOptions{}))
	need[*Result](t)(p.Click(ctx, find(t, s, "radio", "Large"), ClickOptions{}))
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Note"), "line one", TypeOptions{}))
	need[*Result](t)(p.Type(ctx, find(t, s, "date", "Day"), "2026-11-20", TypeOptions{}))
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Editor"), "rich text", TypeOptions{}))

	s = snap(t, p, ctx)
	if !strings.Contains(s.String(), `"Password" value="`) || strings.Contains(s.String(), "s3cret") {
		t.Fatalf("password must be masked:\n%s", s)
	}
	if !strings.Contains(s.String(), `"Editor" value="rich text"`) {
		t.Fatalf("contenteditable value missing:\n%s", s)
	}
	need[*Result](t)(p.Click(ctx, find(t, s, "button", "Send"), ClickOptions{}))
	text(t, p, ctx, "submitted: name=Ada Lovelace&pw=s3cret&city=lon&terms=yes&size=l&note=line one&day=2026-11-20")

	// Replace (default) vs append.
	s = snap(t, p, ctx)
	name := find(t, s, "textbox", "Full name")
	need[*Result](t)(p.Type(ctx, name, " Byron", TypeOptions{Append: true}))
	need[*Result](t)(p.Type(ctx, name, "Grace", TypeOptions{}))
	s = snap(t, p, ctx)
	if !strings.Contains(s.String(), `"Full name" value="Grace"`) {
		t.Fatalf("replace failed:\n%s", s)
	}
}

func TestShadowDOM(t *testing.T) {
	p, ctx := open(t, "/shadow")
	s := snap(t, p, ctx)
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Shadow input"), "inside", TypeOptions{}))
	need[*Result](t)(p.Click(ctx, find(t, s, "button", "Shadow button"), ClickOptions{}))
	text(t, p, ctx, "1")
	s = snap(t, p, ctx)
	if !strings.Contains(s.String(), `"Shadow input" value="inside"`) {
		t.Fatalf("shadow input value:\n%s", s)
	}
}

func TestFramesIncludingCrossSite(t *testing.T) {
	p, ctx := open(t, "/frames")
	var s *Snapshot
	for i := 0; i < 30; i++ { // cross-site frame attaches asynchronously
		s = snap(t, p, ctx)
		if strings.Contains(s.String(), "Cross button") && strings.Contains(s.String(), "Inner button") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	inner := find(t, s, "button", "Inner button")
	cross := find(t, s, "button", "Cross button")
	if !strings.HasPrefix(inner, "f") || !strings.HasPrefix(cross, "f") || inner[:2] == cross[:2] {
		t.Fatalf("frame refs: inner=%s cross=%s\n%s", inner, cross, s)
	}
	p.mu.Lock()
	n := len(p.oopif)
	p.mu.Unlock()
	if n == 0 {
		t.Log("note: cross-site iframe was in-process (no OOPIF); coordinates still verified")
	}
	need[*Result](t)(p.Click(ctx, inner, ClickOptions{}))
	need[*Result](t)(p.Type(ctx, find(t, s, "textbox", "Cross input"), "hi from parent", TypeOptions{}))
	need[*Result](t)(p.Click(ctx, cross, ClickOptions{}))
	text(t, p, ctx, "inner clicked")
	text(t, p, ctx, "cross clicked: hi from parent")
}

func TestClickableDiv(t *testing.T) {
	p, ctx := open(t, "/clickable")
	s := snap(t, p, ctx)
	need[*Result](t)(p.Click(ctx, find(t, s, "clickable", "Open panel"), ClickOptions{}))
	text(t, p, ctx, "panel opened")
}

func TestCoveredTargetIsRefused(t *testing.T) {
	p, ctx := open(t, "/covered")
	s := snap(t, p, ctx)
	_, err := p.Click(ctx, find(t, s, "button", "Hidden target"), ClickOptions{})
	if err == nil || !strings.Contains(err.Error(), "covered by <div#overlay>") {
		t.Fatalf("want covered error, got %v", err)
	}
}

func TestDialogs(t *testing.T) {
	p, ctx := open(t, "/dialogs")
	s := snap(t, p, ctx)

	r := need[*Result](t)(p.Click(ctx, find(t, s, "button", "Alert"), ClickOptions{}))
	if r.Dialog == nil || r.Dialog.Type != "alert" || r.Dialog.Message != "hello alert" {
		t.Fatalf("alert not reported: %+v", r)
	}
	if _, err := p.Click(ctx, find(t, s, "button", "Confirm"), ClickOptions{}); err != ErrDialogOpen {
		t.Fatalf("action during dialog: %v", err)
	}
	if ds := snap(t, p, ctx); ds.Dialog == nil {
		t.Fatal("snapshot should report the dialog")
	}
	need[*Dialog](t)(p.HandleDialog(ctx, true, ""))
	text(t, p, ctx, "after alert")

	r = need[*Result](t)(p.Click(ctx, find(t, s, "button", "Confirm"), ClickOptions{}))
	if r.Dialog == nil || r.Dialog.Type != "confirm" {
		t.Fatalf("confirm: %+v", r)
	}
	need[*Dialog](t)(p.HandleDialog(ctx, false, ""))
	text(t, p, ctx, "cancelled")

	need[*Result](t)(p.Click(ctx, find(t, s, "button", "Prompt"), ClickOptions{}))
	need[*Dialog](t)(p.HandleDialog(ctx, true, "Grace"))
	text(t, p, ctx, "prompt: Grace")
}

func TestUpload(t *testing.T) {
	p, ctx := open(t, "/upload")
	f := filepath.Join(t.TempDir(), "report.txt")
	os.WriteFile(f, []byte("hi"), 0o600)
	s := snap(t, p, ctx)
	need[*Result](t)(p.Upload(ctx, find(t, s, "file", "Direct file"), []string{f}))
	text(t, p, ctx, "direct: report.txt")
	need[*Result](t)(p.Upload(ctx, find(t, s, "button", "Choose file"), []string{f}))
	text(t, p, ctx, "chooser: report.txt")
}

func TestDrag(t *testing.T) {
	p, ctx := open(t, "/drag")
	s := snap(t, p, ctx)
	need[*Result](t)(p.Drag(ctx, find(t, s, "", "Item"), find(t, s, "", "Drop zone")))
	text(t, p, ctx, "dropped item")
	knob := find(t, s, "", "Knob")
	var e Element
	for _, x := range s.Elements {
		if x.Ref == knob {
			e = x
		}
	}
	cx, cy := e.Rect[0]+e.Rect[2]/2, e.Rect[1]+e.Rect[3]/2
	need[*Result](t)(p.DragXY(ctx, cx, cy, cx+100, cy))
	text(t, p, ctx, "knob moved 100")
}

func TestHoverRevealsMenu(t *testing.T) {
	p, ctx := open(t, "/hover")
	s := snap(t, p, ctx)
	if strings.Contains(s.String(), "Gadgets") {
		t.Fatal("menu visible before hover")
	}
	need[*Result](t)(p.Hover(ctx, find(t, s, "button", "Products")))
	s = snap(t, p, ctx)
	need[*Result](t)(p.Click(ctx, find(t, s, "link", "Gadgets"), ClickOptions{}))
}

func TestKeyboard(t *testing.T) {
	p, ctx := open(t, "/keys")
	s := snap(t, p, ctx)
	a := find(t, s, "textbox", "A")
	need[*Result](t)(p.Type(ctx, a, "hello", TypeOptions{}))
	if runtime.GOOS == "darwin" {
		need[*Result](t)(p.Key(ctx, "Meta+A"))
	} else {
		need[*Result](t)(p.Key(ctx, "Control+A"))
	}
	need[*Result](t)(p.KeyType(ctx, "x"))
	s = snap(t, p, ctx)
	if !strings.Contains(s.String(), `"A" value="x"`) {
		t.Fatalf("select-all + type failed:\n%s", s)
	}
	need[*Result](t)(p.Key(ctx, "Tab"))
	text(t, p, ctx, "B focused")
}

func TestNestedScroll(t *testing.T) {
	p, ctx := open(t, "/scroll")
	s := snap(t, p, ctx)
	deep := find(t, s, "button", "Deep button")
	for _, e := range s.Elements {
		if e.Ref == deep && e.InView {
			t.Fatal("deep button should start out of view")
		}
	}
	need[*Result](t)(p.Click(ctx, deep, ClickOptions{})) // auto-scrolls the container
	text(t, p, ctx, "deep clicked")
	r := need[*Result](t)(p.Scroll(ctx, find(t, s, "", "Scroller"), 0, -2000))
	_ = r
	var top float64
	v := need[jsonv2.Value](t)(p.Eval(ctx, "document.getElementById('box').scrollTop"))
	fmt.Sscan(string(v), &top)
	if top != 0 {
		t.Fatalf("wheel scroll did not reach top: %v", top)
	}
}

func TestNavigationIsReported(t *testing.T) {
	p, ctx := open(t, "/nav")
	s := snap(t, p, ctx)
	r := need[*Result](t)(p.Click(ctx, find(t, s, "link", "Next page"), ClickOptions{}))
	if !r.Navigated || r.Title != "nav two" {
		t.Fatalf("navigation result: %+v", r)
	}
	r = need[*Result](t)(p.History(ctx, -1))
	if r.Title != "nav one" {
		t.Fatalf("back: %+v", r)
	}
}

func TestEmulation(t *testing.T) {
	p, ctx := open(t, "/nav")
	lat, lon := 47.37, 8.54
	if err := p.Emulate(ctx, Emulation{Width: 393, Height: 852, Scale: 3, Mobile: true, Touch: true, Latitude: &lat, Longitude: &lon, Timezone: "Europe/Zurich", Locale: "de-CH", ColorScheme: "dark"}); err != nil {
		t.Fatal(err)
	}
	v := need[jsonv2.Value](t)(p.Eval(ctx, `new Promise(r => navigator.geolocation.getCurrentPosition(
		pos => r([screen.width, devicePixelRatio, Intl.DateTimeFormat().resolvedOptions().timeZone, Intl.DateTimeFormat().resolvedOptions().locale,
		         matchMedia('(prefers-color-scheme: dark)').matches, navigator.maxTouchPoints > 0, pos.coords.latitude]),
		err => r(['geo error', err.message]), {timeout: 3000}))`))
	want := `[393,3,"Europe/Zurich","de-CH",true,true,47.37]`
	if string(v) != want {
		t.Fatalf("emulation: got %s want %s", v, want)
	}
	if err := p.Emulate(ctx, Emulation{Reset: true}); err != nil {
		t.Fatal(err)
	}
	v = need[jsonv2.Value](t)(p.Eval(ctx, "screen.width > 393"))
	if string(v) != "true" {
		t.Fatalf("reset did not restore viewport: %s", v)
	}
}

func TestStateSaveLoad(t *testing.T) {
	p, ctx := open(t, "/nav")
	need[jsonv2.Value](t)(p.Eval(ctx, `document.cookie = "session=abc123; path=/"; localStorage.setItem("theme", "dark"); 1`))
	path := filepath.Join(t.TempDir(), "state.json")
	if n, err := p.SaveState(ctx, path); err != nil || n == 0 {
		t.Fatalf("save: n=%d err=%v", n, err)
	}
	need[jsonv2.Value](t)(p.Eval(ctx, `document.cookie = "session=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/"; localStorage.clear(); 1`))
	if _, err := p.LoadState(ctx, path); err != nil {
		t.Fatal(err)
	}
	v := need[jsonv2.Value](t)(p.Eval(ctx, `[document.cookie.includes("session=abc123"), localStorage.getItem("theme")]`))
	if string(v) != `[true,"dark"]` {
		t.Fatalf("restore: %s", v)
	}
}

func TestStaleRef(t *testing.T) {
	p, ctx := open(t, "/nav")
	s := snap(t, p, ctx)
	link := find(t, s, "link", "Next page")
	need[*Result](t)(p.Navigate(ctx, shared.base+"/nav2"))
	if _, err := p.Click(ctx, link, ClickOptions{}); err == nil || !strings.Contains(err.Error(), "stale ref") {
		t.Fatalf("want stale ref error, got %v", err)
	}
}
