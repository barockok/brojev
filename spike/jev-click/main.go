// Command jev-click is a spike: can Jev locate the Cloudflare Turnstile
// "Verify you are human" checkbox from a screenshot alone, and does a real
// click at the point it returns produce a token?
//
// It runs only against Cloudflare's published always-interactive test sitekey
// 3x00000000000000000000FF on a local fixture page. It never targets a real
// site.
//
// Run:
//
//	set -a; . ~/.brojev/env; set +a
//	go run ./spike/jev-click -runs 10
//
// Requires TYPESAFE_API_KEY (or JEV_API_KEY). Headed Chrome by default.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/page"
)

const (
	sitekey = "3x00000000000000000000FF"
	fixture = `<!doctype html><meta charset=utf-8><title>turnstile spike</title>
<style>
 body{font-family:-apple-system,system-ui,sans-serif;padding:48px;max-width:760px}
 h1{font-size:22px} .cf-turnstile{margin:24px 0}
</style>
<h1>Jev checkbox spike</h1>
<p>Cloudflare Turnstile test key 3x (always interactive).</p>
<div class="cf-turnstile" id="ts"></div>
<p id=status>waiting</p>
<script>
 window.onToken = function(t){document.getElementById('status').textContent='token:'+t};
 window.onTsError = function(e){document.getElementById('status').textContent='error:'+e};
 window.onerror = function(m){document.getElementById('status').textContent='jserror:'+m};
</script>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit"></script>
<script>
 try {
   window.turnstile.render('#ts', {
     sitekey: '` + sitekey + `',
     callback: window.onToken,
     'error-callback': window.onTsError
   });
 } catch(e) { document.getElementById('status').textContent='render-exception:'+e; }
</script>`
)

type rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type frameNode struct {
	Frame struct {
		URL string `json:"url"`
	} `json:"frame"`
	ChildFrames []frameNode `json:"childFrames"`
}

type jevBox struct {
	Top    float64 `json:"top"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
	Right  float64 `json:"right"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Noul          float64            `json:"noul"`
	Box           *jevBox            `json:"box"`
}

type jevUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   jevUsage             `json:"usage"`
}

type record struct {
	Run          int      `json:"run"`
	Model        string   `json:"model"`
	Frames       []string `json:"frames,omitempty"`
	DOM          string   `json:"dom,omitempty"`
	JevMS        int64    `json:"jev_ms"`
	InputTokens  int      `json:"input_tokens"`
	CheckboxBox  *jevBox  `json:"checkbox_box,omitempty"`
	CheckboxConf float64  `json:"checkbox_confidence"`
	WidgetBox    *jevBox  `json:"widget_box,omitempty"`
	WidgetConf   float64  `json:"widget_confidence"`
	Present      bool     `json:"checkbox_present"`
	PresentP     float64  `json:"checkbox_present_p"`
	State        string   `json:"widget_state"`
	Clip         rect     `json:"clip"`
	Scale        float64  `json:"scale"`
	ImageW       int      `json:"image_w"`
	ImageH       int      `json:"image_h"`
	ClickX       float64  `json:"click_x"`
	ClickY       float64  `json:"click_y"`
	InsideWidget bool     `json:"click_inside_widget"`
	Token        bool     `json:"token"`
	TokenBefore  bool     `json:"token_before_click"`
	TokenWaitMS  int64    `json:"token_wait_ms"`
	Screenshot   string   `json:"screenshot"`
	Predicted    string   `json:"predicted_crop"`
	Error        string   `json:"error,omitempty"`
}

func main() {
	runs := flag.Int("runs", 10, "number of fresh challenges")
	headless := flag.Bool("headless", false, "run Chrome headless")
	outFlag := flag.String("out", "", "output directory (default: system temp)")
	model := flag.String("model", envOr("TYPESAFE_MODEL", "jev-latest"), "Jev model")
	endpoint := flag.String("endpoint", "https://api.typesafe.ai/v1/systemone", "System One endpoint")
	scale := flag.Float64("scale", 2, "screenshot scale")
	flag.Parse()

	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		key = os.Getenv("JEV_API_KEY")
	}
	if key == "" {
		fatalf("TYPESAFE_API_KEY or JEV_API_KEY is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, fixture)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	out := *outFlag
	if out == "" {
		out = filepath.Join(os.TempDir(), "jev-turnstile-spike-"+time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatalf("output dir: %v", err)
	}
	profile, err := os.MkdirTemp("", "jev-turnstile-chrome-")
	if err != nil {
		fatalf("temp profile: %v", err)
	}
	defer os.RemoveAll(profile)

	ep, _, err := chrome.Ensure(ctx, chrome.Options{
		Profile:  filepath.Join(profile, "profile"),
		Headless: *headless,
		Log:      filepath.Join(out, "chrome.log"),
	})
	if err != nil {
		fatalf("chrome: %v", err)
	}
	b, err := browser.Connect(ctx, ep)
	if err != nil {
		fatalf("connect: %v", err)
	}
	defer func() {
		b.Conn().Browser().Call(context.Background(), "Browser.close", nil, nil)
		b.Close()
	}()

	client := &http.Client{Timeout: 45 * time.Second}
	var recs []record
	for i := 1; i <= *runs; i++ {
		rec := runOnce(ctx, b, client, key, *endpoint, *model, base, out, i, *scale)
		recs = append(recs, rec)
		blob, _ := json.Marshal(rec)
		fmt.Println(string(blob))
		time.Sleep(500 * time.Millisecond)
	}

	summarize(recs, out)
}

func runOnce(ctx context.Context, b *browser.Browser, client *http.Client, key, endpoint, model, base, out string, run int, scale float64) record {
	rec := record{Run: run, Screenshot: filepath.Join(out, fmt.Sprintf("run%02d.png", run)), Predicted: filepath.Join(out, fmt.Sprintf("run%02d-predicted.png", run))}
	info, err := b.Open(ctx, base)
	if err != nil {
		rec.Error = "open: " + err.Error()
		return rec
	}
	tab, err := b.Attach(ctx, target.ID(info.TargetID))
	if err != nil {
		rec.Error = "attach: " + err.Error()
		return rec
	}
	defer b.CloseTab(context.Background(), tab.ID)

	p, err := page.New(ctx, tab.ID, tab.Session)
	if err != nil {
		rec.Error = "page: " + err.Error()
		return rec
	}
	defer p.Close()
	if err := p.Front(ctx); err != nil {
		rec.Error = "front: " + err.Error()
		return rec
	}

	wr, err := waitWidget(ctx, p)
	if err != nil {
		rec.Error = err.Error()
		if d, derr := p.Eval(ctx, `(() => ({
			href: location.href,
			title: document.title,
			status: (document.getElementById("status")||{}).textContent,
			text: ((document.body && document.body.innerText) || "").slice(0,300),
			widgets: document.querySelectorAll(".cf-turnstile").length,
			iframes: [...document.querySelectorAll("iframe")].map(f=>f.src).slice(0,5),
			turnstile: typeof window.turnstile,
			render: typeof (window.turnstile && window.turnstile.render),
			inner: ((document.getElementById("ts")||{}).innerHTML || "").slice(0,300),
			scripts: [...document.scripts].map(s=>s.src).filter(Boolean).slice(0,5)
		}))()`); derr == nil {
			rec.Error += " diag=" + string(d)
		}
		if shot, serr := p.Screenshot(ctx, true); serr == nil {
			os.WriteFile(filepath.Join(out, fmt.Sprintf("run%02d-timeout.png", run)), shot, 0o644)
		}
		return rec
	}
	rec.Frames = frameURLs(ctx, tab)
	if run == 1 {
		rec.DOM = domProbe(ctx, p)
	}

	var vp struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if v, err := p.Eval(ctx, `({w:window.innerWidth,h:window.innerHeight})`); err == nil {
		json.Unmarshal([]byte(v), &vp)
	}
	if vp.W == 0 || vp.H == 0 {
		rec.Error = "viewport unavailable"
		return rec
	}

	x0 := math.Max(0, wr.X-40)
	y0 := math.Max(0, wr.Y-40)
	x1 := math.Min(vp.W, wr.X+math.Min(wr.W, 380)+40)
	y1 := math.Min(vp.H, wr.Y+math.Min(wr.H, 150)+40)
	rec.Clip = rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}

	effScale := scale
	if s := 256 / math.Min(rec.Clip.W, rec.Clip.H); effScale < s {
		effScale = s
	}
	if s := 1568 / math.Max(rec.Clip.W, rec.Clip.H); effScale > s {
		effScale = s
	}
	rec.Scale = effScale
	rec.ImageW = int(math.Round(rec.Clip.W * effScale))
	rec.ImageH = int(math.Round(rec.Clip.H * effScale))

	shot, err := captureClip(ctx, tab, rec.Clip, effScale)
	if err != nil {
		rec.Error = "screenshot: " + err.Error()
		return rec
	}
	if err := os.WriteFile(rec.Screenshot, shot, 0o644); err != nil {
		rec.Error = "write screenshot: " + err.Error()
		return rec
	}

	resp, ms, err := askJev(ctx, client, key, endpoint, model, shot)
	rec.JevMS = ms.Milliseconds()
	if err != nil {
		rec.Error = "jev: " + err.Error()
		return rec
	}
	rec.Model = resp.Model
	rec.InputTokens = resp.Usage.InputTokens
	if a, ok := resp.Answers["checkbox"]; ok {
		rec.CheckboxBox, rec.CheckboxConf = a.Box, a.Confidence
	}
	if a, ok := resp.Answers["widget"]; ok {
		rec.WidgetBox, rec.WidgetConf = a.Box, a.Confidence
	}
	if a, ok := resp.Answers["checkbox_present"]; ok {
		rec.Present, rec.PresentP = a.Noul >= 0.5, a.Noul
	}
	if a, ok := resp.Answers["state"]; ok {
		rec.State = a.Choice
	}
	if rec.CheckboxBox == nil {
		rec.Error = "jev returned no checkbox box"
		return rec
	}

	cx := rec.Clip.X + (rec.CheckboxBox.Left+rec.CheckboxBox.Right)/2*rec.Clip.W
	cy := rec.Clip.Y + (rec.CheckboxBox.Top+rec.CheckboxBox.Bottom)/2*rec.Clip.H
	rec.ClickX, rec.ClickY = cx, cy
	rec.InsideWidget = cx >= wr.X && cx <= wr.X+wr.W && cy >= wr.Y && cy <= wr.Y+wr.H

	if crop, err := cropPNG(shot, *rec.CheckboxBox); err == nil {
		os.WriteFile(rec.Predicted, crop, 0o644)
	}

	rec.TokenBefore = tokenValue(ctx, p) != ""

	if _, err := p.ClickXY(ctx, cx, cy, page.ClickOptions{}); err != nil {
		rec.Error = "click: " + err.Error()
		return rec
	}

	start := time.Now()
	tok, err := waitToken(ctx, p, 10*time.Second)
	rec.TokenWaitMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Error = "token: " + err.Error()
		return rec
	}
	rec.Token = tok != ""
	return rec
}

func waitWidget(ctx context.Context, p *page.Page) (rect, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v, err := p.Eval(ctx, `(() => {
			const host=document.querySelector('.cf-turnstile');
			if(!host) return null;
			const w=host.firstElementChild || host;
			const r=w.getBoundingClientRect();
			if(r.width<100||r.height<30) return null;
			return {x:r.x,y:r.y,w:r.width,h:r.height};
		})()`)
		if err == nil && len(v) > 0 && string(v) != "null" {
			var r rect
			if json.Unmarshal([]byte(v), &r) == nil && r.W > 0 {
				time.Sleep(1200 * time.Millisecond)
				return r, nil
			}
		}
		select {
		case <-ctx.Done():
			return rect{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return rect{}, fmt.Errorf("turnstile iframe did not appear within 30s (test key %s)", sitekey)
}

func domProbe(ctx context.Context, p *page.Page) string {
	v, err := p.Eval(ctx, `(() => {
		const host=document.getElementById('ts');
		const first=host && host.firstElementChild;
		const info={
			hostChildren: host ? host.children.length : 0,
			openShadow: !!(first && first.shadowRoot),
			elements: 0,
			iframes: 0,
			openRoots: 0,
		};
		const walk=(root)=>{
			for(const el of root.querySelectorAll('*')){
				info.elements++;
				if(el.tagName==='IFRAME') info.iframes++;
				if(el.shadowRoot){ info.openRoots++; walk(el.shadowRoot); }
			}
		};
		walk(document);
		info.inner=(first ? first.outerHTML : '').slice(0,400);
		return info;
	})()`)
	if err != nil {
		return "probe error: " + err.Error()
	}
	return string(v)
}

func frameURLs(ctx context.Context, tab *browser.Tab) []string {
	var res struct {
		FrameTree frameNode `json:"frameTree"`
	}
	if err := tab.Session.Call(ctx, "Page.getFrameTree", nil, &res); err != nil {
		return nil
	}
	var out []string
	var walk func(frameNode)
	walk = func(n frameNode) {
		if n.Frame.URL != "" {
			out = append(out, n.Frame.URL)
		}
		for _, c := range n.ChildFrames {
			walk(c)
		}
	}
	walk(res.FrameTree)
	return out
}

func captureClip(ctx context.Context, tab *browser.Tab, r rect, scale float64) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	err := tab.Session.Call(ctx, "Page.captureScreenshot", map[string]any{
		"format":      "png",
		"fromSurface": true,
		"clip": map[string]any{
			"x": r.X, "y": r.Y, "width": r.W, "height": r.H, "scale": scale,
		},
	}, &res)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

func askJev(ctx context.Context, client *http.Client, key, endpoint, model string, img []byte) (*jevResponse, time.Duration, error) {
	ref := "img" + randomHex(8)
	body := map[string]any{
		"model": model,
		"state": map[string]any{"shot": ref},
		"assets": map[string]any{
			ref: map[string]any{"type": "image", "format": "png", "data_base64": base64.StdEncoding.EncodeToString(img)},
		},
		"questions": map[string]any{
			"checkbox": map[string]any{
				"type":         "box",
				"instructions": "the small square checkbox immediately to the left of the words \"Verify you are human\" in `shot`",
			},
			"widget": map[string]any{
				"type":         "box",
				"instructions": "the entire Cloudflare verification widget, including its checkbox, text and logo, in `shot`",
			},
			"checkbox_present": map[string]any{
				"type":         "noul",
				"instructions": "Is an empty square checkbox next to the words \"Verify you are human\" visible in `shot`?",
			},
			"state": map[string]any{
				"type":         "choice",
				"instructions": "What is the Cloudflare verification widget in `shot` showing?",
				"criteria": map[string]string{
					"checkbox":        "An empty square checkbox next to the text Verify you are human, waiting for a click.",
					"spinner":         "A spinner or loading indicator, the widget is checking.",
					"verified":        "A green checkmark, or text saying Success or Verified.",
					"image_challenge": "A grid of images, or a request to select images.",
					"other":           "Anything else, including an error or a widget that failed to load.",
				},
			},
		},
	}
	blob, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(blob))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Since(start), err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, time.Since(start), fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 400))
	}
	var r jevResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, time.Since(start), fmt.Errorf("bad response: %w: %s", err, truncate(string(data), 300))
	}
	return &r, time.Since(start), nil
}

func waitToken(ctx context.Context, p *page.Page, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := tokenValue(ctx, p); s != "" {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return "", nil
}

func tokenValue(ctx context.Context, p *page.Page) string {
	v, err := p.Eval(ctx, `(() => {
		const e=document.querySelector('input[name="cf-turnstile-response"]');
		return e && e.value ? e.value : "";
	})()`)
	if err != nil {
		return ""
	}
	var s string
	if json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return ""
}

func cropPNG(data []byte, b jevBox) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	w, h := float64(bounds.Dx()), float64(bounds.Dy())
	x0 := int(math.Floor(b.Left*w)) - 8
	y0 := int(math.Floor(b.Top*h)) - 8
	x1 := int(math.Ceil(b.Right*w)) + 8
	y1 := int(math.Ceil(b.Bottom*h)) + 8
	x0 = max(0, min(x0, bounds.Dx()-1))
	y0 = max(0, min(y0, bounds.Dy()-1))
	x1 = max(x0+1, min(x1, bounds.Dx()))
	y1 = max(y0+1, min(y1, bounds.Dy()))
	dst := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	draw.Draw(dst, dst.Bounds(), img, image.Pt(bounds.Min.X+x0, bounds.Min.Y+y0), draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func summarize(recs []record, out string) {
	var boxes, inside, success int
	var confs []float64
	var lat []int64
	var inputTokens int
	for _, r := range recs {
		inputTokens += r.InputTokens
		if r.CheckboxBox != nil {
			boxes++
			confs = append(confs, r.CheckboxConf)
			lat = append(lat, r.JevMS)
		}
		if r.InsideWidget {
			inside++
		}
		if r.Token {
			success++
		}
	}
	var meanConf, medConf, meanMS float64
	if len(confs) > 0 {
		sort.Float64s(confs)
		var sum float64
		for _, c := range confs {
			sum += c
		}
		meanConf = sum / float64(len(confs))
		medConf = confs[len(confs)/2]
		var lsum int64
		for _, v := range lat {
			lsum += v
		}
		meanMS = float64(lsum) / float64(len(lat))
	}
	fmt.Printf("SUMMARY runs=%d boxes=%d inside_widget=%d tokens=%d success_rate=%.0f%% mean_conf=%.2f median_conf=%.2f mean_jev_ms=%.0f input_tokens=%d cost_usd=%.6f out=%s\n",
		len(recs), boxes, inside, success, 100*float64(success)/float64(max(1, len(recs))),
		meanConf, medConf, meanMS, inputTokens, float64(inputTokens)*0.042/1e6, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
