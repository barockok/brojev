# Cloudflare Turnstile Detector and Resolver Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Detect an unsolved Cloudflare Turnstile on any page brojev drives and resolve it transparently: real in-page input first, CapSolver fallback, status attached to existing replies. No new MCP tool and no Turnstile step for the calling agent.

**Architecture:** A new `internal/turnstile` package holds the status model, the `Solver` interface and the CapSolver HTTP client. `internal/page/turnstile.go` holds detection and the resolver, reusing brojev's frame enumeration, isolated-world eval, frame offsets and real CDP mouse input. The daemon, the Jev loop and MCP rendering call `EnsureTurnstile` at the agreed hooks. A missing `CAPSOLVER_API_KEY` degrades to in-page only.

**Tech Stack:** Go 1.25, `chromedp/cdproto` (existing), net/http, Cloudflare Turnstile test sitekeys, CapSolver `AntiTurnstileTaskProxyLess`.

**Design doc:** `docs/plans/2026-11-20-turnstile-design.md`

---

## Conventions

- Work on branch `feat/turnstile-detector-resolver`.
- Run unit tests after every task: `go test ./...`
- Browser tests need `BROJEV_E2E=1`. The Turnstile fixtures need network access to `challenges.cloudflare.com`. If headless Chrome is rejected by Cloudflare, run the browser tests with `BROJEV_HEADED=1`.
- Commit after every task. No em-dashes in commit messages or docs.

---

## Task 1: Status model and CapSolver client

**Files:**
- Create: `internal/turnstile/status.go`
- Create: `internal/turnstile/solver.go`
- Create: `internal/turnstile/capsolver.go`
- Create: `internal/turnstile/capsolver_test.go`

**Step 1: Write the failing tests**

`internal/turnstile/capsolver_test.go`:

```go
package turnstile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCapSolverSolve(t *testing.T) {
	var creates, polls int
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/createTask":
			creates++
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"errorId":0,"status":"idle","taskId":"task-1"}`))
		case "/getTaskResult":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"errorId":0,"status":"processing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"errorId":0,"status":"ready","solution":{"token":"tok-123"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &CapSolver{Key: "key-1", Endpoint: srv.URL, HTTP: srv.Client(), Poll: time.Millisecond}
	tok, err := c.Solve(context.Background(), Target{URL: "https://example.com/login", Sitekey: "site-1", Action: "login", CData: "cd-1"})
	if err != nil || tok != "tok-123" {
		t.Fatalf("Solve = %q, %v", tok, err)
	}
	if creates != 1 || polls != 2 {
		t.Fatalf("creates=%d polls=%d", creates, polls)
	}
	task := createBody["task"].(map[string]any)
	if task["type"] != "AntiTurnstileTaskProxyLess" || task["websiteURL"] != "https://example.com/login" || task["websiteKey"] != "site-1" {
		t.Fatalf("task = %#v", task)
	}
	md, _ := task["metadata"].(map[string]any)
	if md["action"] != "login" || md["cdata"] != "cd-1" {
		t.Fatalf("metadata = %#v", md)
	}
}

func TestCapSolverAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorId":1,"errorCode":"ERROR_KEY_DENIED_ACCESS","errorDescription":"denied"}`))
	}))
	defer srv.Close()
	c := &CapSolver{Key: "bad", Endpoint: srv.URL, HTTP: srv.Client(), Poll: time.Millisecond}
	_, err := c.Solve(context.Background(), Target{URL: "https://x.test", Sitekey: "s"})
	if err == nil || !strings.Contains(err.Error(), "ERROR_KEY_DENIED_ACCESS") {
		t.Fatalf("err = %v", err)
	}
}

func TestCapSolverTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "createTask") {
			_, _ = w.Write([]byte(`{"errorId":0,"taskId":"task-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"errorId":0,"status":"processing"}`))
	}))
	defer srv.Close()
	c := &CapSolver{Key: "k", Endpoint: srv.URL, HTTP: srv.Client(), Poll: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Solve(ctx, Target{URL: "https://x.test", Sitekey: "s"})
	if err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestCapSolverRequiresSitekey(t *testing.T) {
	c := NewCapSolver("k")
	_, err := c.Solve(context.Background(), Target{URL: "https://x.test"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestStatusLine(t *testing.T) {
	cases := []struct {
		s    *Status
		want string
	}{
		{nil, ""},
		{&Status{State: StateAbsent}, ""},
		{&Status{Detected: true, State: StateSolved}, "TURNSTILE: solved"},
		{&Status{Detected: true, State: StateUnresolved, Reason: "solver disabled"}, "TURNSTILE: unresolved (solver disabled)"},
		{&Status{Detected: true, State: StateInterstitial}, "TURNSTILE: interstitial (full-page Cloudflare challenge)"},
	}
	for _, c := range cases {
		if got := c.s.Line(); got != c.want {
			t.Errorf("Line() = %q, want %q", got, c.want)
		}
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `go test ./internal/turnstile/ -v`
Expected: build failure, package has no `CapSolver`, `Target`, `Status`.

**Step 3: Implement the package**

`internal/turnstile/status.go`:

```go
// Package turnstile detects and resolves Cloudflare Turnstile challenges.
package turnstile

// State is the externally visible state of a Turnstile widget.
type State string

const (
	StateAbsent       State = "absent"
	StateSolved       State = "solved"
	StateUnresolved   State = "unresolved"
	StateInterstitial State = "interstitial"
)

const (
	ModeManaged   = "managed"
	ModeInvisible = "invisible"
	ModeUnknown   = "unknown"
)

// CostPerSolveUSD is CapSolver's Cloudflare Turnstile list price: $1.20 per
// 1,000 tasks, checked 2026-11-20.
const CostPerSolveUSD = 0.0012

// Status is what detection and resolution report. It rides on existing tool
// replies; it is not an action the caller has to take.
type Status struct {
	Detected   bool    `json:"detected"`
	Sitekey    string  `json:"sitekey,omitempty"`
	Action     string  `json:"action,omitempty"`
	CData      string  `json:"cdata,omitempty"`
	Mode       string  `json:"mode,omitempty"`
	State      State   `json:"state"`
	Attempts   int     `json:"attempts,omitempty"`
	SolverUsed bool    `json:"solverUsed,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// Line renders the one line added to snapshots and action replies.
func (s *Status) Line() string {
	if s == nil || !s.Detected {
		return ""
	}
	switch s.State {
	case StateSolved:
		return "TURNSTILE: solved"
	case StateInterstitial:
		return "TURNSTILE: interstitial (full-page Cloudflare challenge)"
	default:
		if s.Reason != "" {
			return "TURNSTILE: unresolved (" + s.Reason + ")"
		}
		return "TURNSTILE: unresolved"
	}
}
```

`internal/turnstile/solver.go`:

```go
package turnstile

import (
	"context"
	"os"
	"strings"
)

// Target is what a solver needs to produce a token.
type Target struct {
	URL     string
	Sitekey string
	Action  string
	CData   string
}

// Solver turns a Turnstile sitekey into a response token.
type Solver interface {
	Solve(ctx context.Context, t Target) (string, error)
}

// FromEnv builds the configured solver, or nil when CAPSOLVER_API_KEY is not
// set. The key comes from the plugin settings, the environment, or
// ~/.brojev/env, in that order, via the daemon key plumbing.
func FromEnv() Solver {
	key := strings.TrimSpace(os.Getenv("CAPSOLVER_API_KEY"))
	if key == "" || strings.HasPrefix(key, "${") {
		return nil
	}
	return NewCapSolver(key)
}
```

`internal/turnstile/capsolver.go`:

```go
package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultEndpoint = "https://api.capsolver.com"

// CapSolver is the CapSolver Turnstile client.
type CapSolver struct {
	Key      string
	Endpoint string
	HTTP     *http.Client
	Poll     time.Duration
}

func NewCapSolver(key string) *CapSolver {
	return &CapSolver{
		Key: key, Endpoint: DefaultEndpoint,
		HTTP: &http.Client{Timeout: 30 * time.Second},
		Poll: 2 * time.Second,
	}
}

type capTask struct {
	Type       string            `json:"type"`
	WebsiteURL string            `json:"websiteURL"`
	WebsiteKey string            `json:"websiteKey"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type capCreate struct {
	ClientKey string  `json:"clientKey"`
	Task      capTask `json:"task"`
}

type capResult struct {
	ClientKey string `json:"clientKey"`
	TaskID    string `json:"taskId"`
}

type capResp struct {
	ErrorID          int    `json:"errorId"`
	ErrorCode        string `json:"errorCode"`
	ErrorDescription string `json:"errorDescription"`
	Status           string `json:"status"`
	TaskID           string `json:"taskId"`
	Solution         *struct {
		Token string `json:"token"`
	} `json:"solution"`
}

// Solve creates one AntiTurnstileTaskProxyLess task and polls until it is
// ready or ctx ends.
func (c *CapSolver) Solve(ctx context.Context, t Target) (string, error) {
	if strings.TrimSpace(c.Key) == "" {
		return "", errors.New("capsolver: API key missing")
	}
	if t.URL == "" || t.Sitekey == "" {
		return "", errors.New("capsolver: websiteURL and websiteKey are required")
	}
	md := map[string]string{}
	if t.Action != "" {
		md["action"] = t.Action
	}
	if t.CData != "" {
		md["cdata"] = t.CData
	}
	var created capResp
	if err := c.post(ctx, "/createTask", capCreate{ClientKey: c.Key, Task: capTask{
		Type: "AntiTurnstileTaskProxyLess", WebsiteURL: t.URL, WebsiteKey: t.Sitekey, Metadata: md,
	}}, &created); err != nil {
		return "", err
	}
	if created.ErrorID != 0 {
		return "", apiError("createTask", created)
	}
	if created.TaskID == "" {
		return "", errors.New("capsolver: createTask returned no taskId")
	}
	poll := c.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		var res capResp
		if err := c.post(ctx, "/getTaskResult", capResult{ClientKey: c.Key, TaskID: created.TaskID}, &res); err != nil {
			return "", err
		}
		if res.ErrorID != 0 {
			return "", apiError("getTaskResult", res)
		}
		if res.Status == "ready" {
			if res.Solution == nil || res.Solution.Token == "" {
				return "", errors.New("capsolver: ready without a token")
			}
			return res.Solution.Token, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *CapSolver) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("capsolver %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("capsolver %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("capsolver %s: bad reply: %w", path, err)
	}
	return nil
}

func apiError(op string, r capResp) error {
	if r.ErrorCode == "" && r.ErrorDescription == "" {
		return fmt.Errorf("capsolver %s: errorId %d", op, r.ErrorID)
	}
	if r.ErrorDescription == "" {
		return fmt.Errorf("capsolver %s: %s", op, r.ErrorCode)
	}
	return fmt.Errorf("capsolver %s: %s (%s)", op, r.ErrorCode, r.ErrorDescription)
}
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/turnstile/ -v`
Expected: PASS, no network calls.

**Step 5: Commit**

```bash
git add internal/turnstile
git commit -m "feat(turnstile): status model and CapSolver client"
```

---

## Task 2: Detection

**Files:**
- Create: `internal/page/turnstile.go`
- Create: `internal/page/turnstile_e2e_test.go`
- Modify: `internal/page/e2e_test.go` (add Turnstile fixtures)

**Step 1: Add fixtures and write the failing E2E test**

In `internal/page/e2e_test.go`, add to `fixtures`:

```go
"/turnstile-pass": `<title>turnstile pass</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>
<div class="cf-turnstile" data-sitekey="1x00000000000000000000AA"></div><p id=out></p>`,
"/turnstile-block": `<title>turnstile block</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>
<div class="cf-turnstile" data-sitekey="2x00000000000000000000AB"></div><p id=out></p>`,
"/turnstile-force": `<title>turnstile force</title>
<script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>
<div class="cf-turnstile" data-sitekey="3x00000000000000000000FF"></div><p id=out></p>`,
```

Create `internal/page/turnstile_e2e_test.go`:

```go
package page

import (
	"context"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/turnstile"
)

// waitWidget polls until Cloudflare's script has rendered a widget. The
// fixture needs network access; skip the test if the widget never loads.
func waitWidget(t *testing.T, p *Page, ctx context.Context) *turnstile.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := p.DetectTurnstile(ctx)
		if st.Detected {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skip("Turnstile widget did not load (challenges.cloudflare.com unreachable or blocked)")
	return nil
}

func TestTurnstileDetect(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	st := waitWidget(t, p, ctx)
	if st.Sitekey != "2x00000000000000000000AB" {
		t.Fatalf("sitekey = %q", st.Sitekey)
	}
	if st.Mode != turnstile.ModeManaged {
		t.Fatalf("mode = %q", st.Mode)
	}
	if st.State != turnstile.StateUnresolved {
		t.Fatalf("state = %q, reason %q", st.State, st.Reason)
	}
	if st.Line() == "" {
		t.Fatal("missing status line")
	}

	// A page with no widget must report absent and cost one eval.
	p2, ctx2 := open(t, "/form")
	if st := p2.DetectTurnstile(ctx2); st.Detected || st.State != turnstile.StateAbsent {
		t.Fatalf("form page: %+v", st)
	}
}
```

**Step 2: Run the test to verify it fails**

Run: `BROJEV_E2E=1 go test ./internal/page/ -run TestTurnstileDetect -v`
Expected: build failure, `DetectTurnstile` undefined.

**Step 3: Implement detection**

Create `internal/page/turnstile.go` with the detection script and `DetectTurnstile`. Full code is at the end of this task.

**Step 4: Run the test to verify it passes**

Run: `BROJEV_E2E=1 go test ./internal/page/ -run TestTurnstileDetect -v`
Expected: PASS. If it skips, the widget did not load; check the fixture with `BROJEV_HEADED=1` and confirm the Cloudflare script responds.

**Step 5: Commit**

```bash
git add internal/page/turnstile.go internal/page/turnstile_e2e_test.go internal/page/e2e_test.go
git commit -m "feat(turnstile): detect widgets and full-page interstitials"
```

### Detection implementation (reference)

`internal/page/turnstile.go`, first slice:

```go
package page

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/barockok/brojev/internal/turnstile"
)

const turnstileDetectJS = `(() => {
  const out = {present:false, interstitial:false, sitekey:"", action:"", cdata:"", token:"", mode:"unknown", widget:null, url:location.href};
  const frames = Array.from(document.querySelectorAll('iframe')).filter(f => (f.src || "").startsWith("https://challenges.cloudflare.com/"));
  const roots = [];
  const push = (el) => { if (el && !roots.includes(el)) roots.push(el); };
  for (const el of document.querySelectorAll('.cf-turnstile')) push(el);
  for (const f of frames) {
    let n = f;
    for (; n && n !== document.body; n = n.parentElement) {
      if (n.classList && n.classList.contains('cf-turnstile')) break;
      if (n.hasAttribute && n.hasAttribute('data-sitekey')) break;
      if (n.querySelector && n.querySelector('[name="cf-turnstile-response"]')) break;
    }
    push(n && n !== document.body ? n : f);
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
  const shown = frames.find(f => { const r = f.getBoundingClientRect(); return r.width > 0 && r.height > 0; });
  if (shown) {
    const r = shown.getBoundingClientRect();
    out.widget = {x:r.x, y:r.y, w:r.width, h:r.height};
    out.mode = "managed";
  } else {
    out.mode = "invisible";
  }
  if (!out.sitekey && shown) {
    const m = /[?&]sitekey=([^&]+)/.exec(shown.src || "");
    if (m) out.sitekey = decodeURIComponent(m[1]);
  }
  return out;
})()`

const turnstileWidgetJS = `(() => {
  const box = (el) => { const r = el.getBoundingClientRect(); return {x:r.x, y:r.y, w:r.width, h:r.height}; };
  const shown = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  let cb = document.querySelector('input[type="checkbox"], [role="checkbox"]');
  if (cb && !shown(cb)) cb = null;
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
	Mode         string  `json:"mode"`
	Widget       *tsRect `json:"widget"`
	URL          string  `json:"url"`
}

type tsInner struct {
	Checkbox *tsRect `json:"checkbox"`
	Text     string  `json:"text"`
}

// detectTurnstile returns the widget model, its frame (when brojev has a
// session for it), and the widget-internal state.
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
	for _, f := range fs[1:] {
		if strings.Contains(f.URL, "challenges.cloudflare.com") && strings.Contains(f.URL, "/turnstile/") {
			wf = f
			break
		}
	}
	if wf == nil {
		for _, f := range fs[1:] {
			if strings.Contains(f.URL, "challenges.cloudflare.com") {
				wf = f
				break
			}
		}
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
```

Do not add unused imports (`math`, `os`, `strconv`, `time`) until the resolution slice uses them; Go will reject unused imports.

---

## Task 3: In-page resolution and solver fallback

**Files:**
- Modify: `internal/page/turnstile.go`
- Modify: `internal/page/turnstile_e2e_test.go`
- Modify: `internal/page/page.go` (add per-page resolver state)

**Step 1: Write the failing tests**

Add to `internal/page/turnstile_e2e_test.go`:

```go
type fakeSolver struct {
	token string
	got   turnstile.Target
}

func (f *fakeSolver) Solve(ctx context.Context, t turnstile.Target) (string, error) {
	f.got = t
	return f.token, nil
}

func TestTurnstileAutoPass(t *testing.T) {
	p, ctx := open(t, "/turnstile-pass")
	waitWidget(t, p, ctx)
	st := p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("pass key: %+v", st)
	}
	if st.SolverUsed {
		t.Fatal("solver used for a passing widget")
	}
}

func TestTurnstileClick(t *testing.T) {
	p, ctx := open(t, "/turnstile-force")
	waitWidget(t, p, ctx)
	st := p.EnsureTurnstile(ctx)
	if st == nil || st.State != turnstile.StateSolved {
		t.Fatalf("interactive key: %+v", st)
	}
}

func TestTurnstileSolverInjection(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	fs := &fakeSolver{token: "test-token"}
	st := p.ensureTurnstile(ctx, turnstileOptions{
		Total: 5 * time.Second, InPage: 300 * time.Millisecond, Solver: fs,
	})
	if st == nil || st.State != turnstile.StateSolved || !st.SolverUsed {
		t.Fatalf("solver: %+v", st)
	}
	if fs.got.Sitekey != "2x00000000000000000000AB" || fs.got.URL == "" {
		t.Fatalf("solver target: %+v", fs.got)
	}
	v, err := p.Eval(ctx, `document.querySelector('[name="cf-turnstile-response"]').value`)
	if err != nil || string(v) != `"test-token"` {
		t.Fatalf("injected token = %s, %v", v, err)
	}
}

func TestTurnstileNoSolverIsReported(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	st := p.ensureTurnstile(ctx, turnstileOptions{Total: 2 * time.Second, InPage: 200 * time.Millisecond})
	if st == nil || st.State != turnstile.StateUnresolved || st.SolverUsed {
		t.Fatalf("no solver: %+v", st)
	}
	if !strings.Contains(st.Reason, "CapSolver") {
		t.Fatalf("reason = %q", st.Reason)
	}
}
```

Add `strings` to the test imports.

**Step 2: Run to verify failure**

Run: `BROJEV_E2E=1 go test ./internal/page/ -run 'TestTurnstile(AutoPass|Click|SolverInjection|NoSolver)' -v`
Expected: build failure, `EnsureTurnstile` and `ensureTurnstile` undefined.

**Step 3: Implement**

Add to `internal/page/page.go`'s `Page` struct:

```go
	tsMu    sync.Mutex
	tsTries map[string]tsAttempt
```

Add to `internal/page/turnstile.go`:

```go
type tsAttempt struct {
	cycles int
	last   time.Time
}

type turnstileOptions struct {
	Total  time.Duration
	InPage time.Duration
	Solver turnstile.Solver
}

func turnstileTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("TURNSTILE_TIMEOUT_MS"))
	if v == "" {
		return 60 * time.Second
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 60 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// EnsureTurnstile detects and, when needed, resolves a Turnstile on this page.
// It is safe to call after every action: an absent widget costs one eval.
// It never returns a hard error; failures are in Status.Reason.
func (p *Page) EnsureTurnstile(ctx context.Context) *turnstile.Status {
	return p.ensureTurnstile(ctx, turnstileOptions{
		Total: turnstileTimeout(), InPage: 8 * time.Second, Solver: turnstile.FromEnv(),
	})
}

func (p *Page) ensureTurnstile(ctx context.Context, o turnstileOptions) *turnstile.Status {
	if p.OpenDialog() != nil {
		return nil
	}
	p.tsMu.Lock()
	defer p.tsMu.Unlock()

	total := o.Total
	if total <= 0 {
		total = 60 * time.Second
	}
	inPage := o.InPage
	if inPage <= 0 {
		inPage = 8 * time.Second
	}
	if inPage > total {
		inPage = total
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	w, wf, inner, err := p.detectTurnstile(ctx)
	if err != nil {
		return &turnstile.Status{State: turnstile.StateUnresolved, Reason: "detection failed: " + err.Error()}
	}
	if w == nil || (!w.Present && !w.Interstitial) {
		return &turnstile.Status{State: turnstile.StateAbsent}
	}
	st := statusOf(w, inner)
	if w.Interstitial || w.Token != "" {
		return st
	}

	key := w.URL + "|" + w.Sitekey
	if p.tsTries == nil || len(p.tsTries) > 64 {
		p.tsTries = map[string]tsAttempt{}
	}
	a := p.tsTries[key]
	st.Attempts = a.cycles
	if a.cycles >= 2 {
		st.Reason = "gave up after 2 attempts"
		return st
	}
	if !a.last.IsZero() && time.Since(a.last) < time.Second {
		if st.Reason == "" {
			st.Reason = "cooling down"
		}
		return st
	}
	a.cycles++
	a.last = time.Now()
	p.tsTries[key] = a
	st.Attempts = a.cycles
	st.Reason = ""

	if p.tryTurnstileInPage(ctx, w, wf, inner, inPage) {
		delete(p.tsTries, key)
		fresh, _, _, _ := p.detectTurnstile(ctx)
		return statusOf(fresh, nil)
	}

	if o.Solver == nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "in-page attempt failed; CapSolver not configured"
		return st
	}
	if w.Sitekey == "" {
		st.State = turnstile.StateUnresolved
		st.Reason = "in-page attempt failed; sitekey missing"
		return st
	}
	st.SolverUsed = true
	st.CostUSD = turnstile.CostPerSolveUSD
	token, err := o.Solver.Solve(ctx, turnstile.Target{URL: w.URL, Sitekey: w.Sitekey, Action: w.Action, CData: w.CData})
	if err != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "solver failed: " + err.Error()
		return st
	}
	if err := p.injectTurnstileToken(ctx, wf, token); err != nil {
		st.State = turnstile.StateUnresolved
		st.Reason = "token injection failed: " + err.Error()
		return st
	}
	fresh, _, freshInner, _ := p.detectTurnstile(ctx)
	final := statusOf(fresh, freshInner)
	final.Attempts, final.SolverUsed, final.CostUSD = st.Attempts, true, st.CostUSD
	if final.State != turnstile.StateSolved {
		final.State = turnstile.StateUnresolved
		final.Reason = "solver returned a token but the page did not accept it"
		return final
	}
	delete(p.tsTries, key)
	return final
}

func (p *Page) tryTurnstileInPage(ctx context.Context, w *tsWidget, wf *frame, inner *tsInner, budget time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	_ = p.Front(ctx)
	_ = p.resetTurnstileWidget(ctx, wf)

	click := func() {
		if inner != nil && inner.Checkbox != nil && wf != nil {
			if p.clickTurnstileCheckbox(ctx, wf, inner.Checkbox) == nil {
				return
			}
		}
		if w.Widget != nil {
			p.clickTurnstileWidget(ctx, w.Widget)
		}
	}
	clicked := false
	click()
	clicked = true
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return p.turnstileHasToken(ctx)
		case <-time.After(250 * time.Millisecond):
		}
		if p.turnstileHasToken(ctx) {
			return true
		}
		if !clicked || time.Since(start) > 2*time.Second {
			clicked = true // one retry only
			click()
		}
	}
}

func (p *Page) turnstileHasToken(ctx context.Context) bool {
	w, _, _, err := p.detectTurnstile(ctx)
	return err == nil && w != nil && w.Token != ""
}

func (p *Page) resetTurnstileWidget(ctx context.Context, wf *frame) error {
	js := `(() => { if (window.turnstile && window.turnstile.reset) { window.turnstile.reset(); return true; } return false; })()`
	if wf != nil {
		_, _ = p.evalMain(ctx, wf, js)
	}
	_, err := p.Eval(ctx, js)
	return err
}

func (p *Page) clickTurnstileCheckbox(ctx context.Context, wf *frame, cb *tsRect) error {
	p.revealFrameChain(ctx, wf)
	fs, err := p.frames(ctx)
	if err != nil {
		return err
	}
	origin, _ := p.offsets(ctx, fs)
	o, ok := origin[wf.ID]
	if !ok {
		return errors.New("turnstile frame is not visible")
	}
	x, y := o.X+cb.X+cb.W/2, o.Y+cb.Y+cb.H/2
	_, err = p.ClickXY(ctx, x, y, ClickOptions{})
	return err
}

func (p *Page) clickTurnstileWidget(ctx context.Context, r *tsRect) {
	fs, err := p.frames(ctx)
	if err != nil || len(fs) == 0 {
		return
	}
	_, _ = p.eval(ctx, fs[0], `(() => {
	  const f = [...document.querySelectorAll('iframe')].find(x => (x.src || "").startsWith("https://challenges.cloudflare.com/"));
	  if (f) f.scrollIntoView({block:'center', inline:'center', behavior:'instant'});
	  return true;
	})()`, nil)
	w, _, _, err := p.detectTurnstile(ctx)
	if err != nil || w.Widget == nil {
		return
	}
	r = w.Widget
	x := r.X + math.Min(24, r.W*0.25)
	y := r.Y + r.H/2
	_, _ = p.ClickXY(ctx, x, y, ClickOptions{})
}

func (p *Page) revealFrameChain(ctx context.Context, f *frame) {
	fs, err := p.frames(ctx)
	if err != nil {
		return
	}
	byID := map[cdproto.FrameID]*frame{}
	for _, x := range fs {
		byID[x.ID] = x
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
}

func (p *Page) injectTurnstileToken(ctx context.Context, wf *frame, token string) error {
	js := `(() => {
	  const token = ` + jsString(token) + `;
	  let el = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
	  if (!el) {
	    const root = document.querySelector('.cf-turnstile') || document.body;
	    el = document.createElement('input');
	    el.type = 'hidden';
	    el.name = 'cf-turnstile-response';
	    root.appendChild(el);
	  }
	  const proto = el.localName === 'textarea' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
	  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, token);
	  el.dispatchEvent(new Event('input', {bubbles:true, composed:true}));
	  el.dispatchEvent(new Event('change', {bubbles:true}));
	  return el.value;
	})()`
	var got string
	var err error
	if wf != nil {
		err = p.evalMain(ctx, wf, js, &got)
	} else {
		var v jsonv2.Value
		v, err = p.Eval(ctx, js)
		got = strings.Trim(string(v), `"`)
	}
	if err != nil {
		return err
	}
	if got != token {
		return errors.New("response input did not take the token")
	}
	return nil
}
```

Add `evalMain` to `internal/page/page.go` or `turnstile.go`:

```go
// evalMain runs expr in f's main world, where page globals and event
// handlers live. Without an explicit contextId, CDP uses the frame's main
// execution context.
func (p *Page) evalMain(ctx context.Context, f *frame, expr string, out any) error {
	yes := true
	res, err := cdproto.Call(ctx, f.sess, runtime.Evaluate, runtime.EvaluateParams{
		Expression: expr, ReturnByValue: &yes, AwaitPromise: &yes, UserGesture: &yes,
	})
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
```

Imports needed in `turnstile.go`: `context`, `errors`, `math`, `os`, `strconv`, `strings`, `time`, `github.com/chromedp/cdproto/cdp` (for `cdproto.FrameID`), `github.com/chromedp/cdproto/cdp/jsonv2`, `github.com/chromedp/cdproto/runtime`, and `internal/turnstile`.

**Step 4: Run tests**

Run: `BROJEV_E2E=1 go test ./internal/page/ -run 'TestTurnstile' -v`
Expected: PASS. If `TestTurnstileClick` fails because Cloudflare rejects headless, re-run with `BROJEV_HEADED=1`. If the click targets the wrong spot, inspect the widget frame DOM with a temporary `p.eval` print and adjust the fallback point.

**Step 5: Commit**

```bash
git add internal/page
git commit -m "feat(turnstile): in-page click, wait and CapSolver fallback"
```

---

## Task 4: Snapshot status line

**Files:**
- Modify: `internal/page/snapshot.go`
- Modify: `internal/page/turnstile_e2e_test.go`

**Step 1: Write the failing test**

Add:

```go
func TestTurnstileSnapshotLine(t *testing.T) {
	p, ctx := open(t, "/turnstile-block")
	waitWidget(t, p, ctx)
	st := p.DetectTurnstile(ctx)
	s := snap(t, p, ctx)
	s.Turnstile = st
	if !strings.Contains(s.String(), "TURNSTILE: unresolved") {
		t.Fatalf("snapshot:\n%s", s)
	}
}
```

**Step 2: Run and see it fail**

Run: `BROJEV_E2E=1 go test ./internal/page/ -run TestTurnstileSnapshotLine -v`
Expected: build failure, `Snapshot` has no `Turnstile` field.

**Step 3: Implement**

- Import `internal/turnstile` in `snapshot.go`.
- Add `Turnstile *turnstile.Status \`json:"turnstile,omitempty"\`` to `Snapshot`.
- In `String()`, after the title line, write `s.Turnstile.Line()` when non-empty.

**Step 4: Run**

`BROJEV_E2E=1 go test ./internal/page/ -run TestTurnstile -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/page/snapshot.go internal/page/turnstile_e2e_test.go
git commit -m "feat(turnstile): show widget state in snapshots"
```

---

## Task 5: Daemon hooks and key forwarding

**Files:**
- Modify: `internal/daemon/tools.go`
- Modify: `internal/daemon/keys.go`
- Modify: `internal/daemon/keys_test.go`
- Modify: `internal/mcpserver/server.go`

**Step 1: Write failing tests**

- In `internal/daemon/keys_test.go`, assert `CAPSOLVER_API_KEY` is forwarded and cleared like the other keys.
- Add a small unit test for the MCP key fingerprint helper if needed. The daemon browser hook is covered by the existing MCP E2E once primitives run, but add an assertion in `TestMCPEndToEnd` later.

**Step 2: Run**

`go test ./internal/daemon/ ./internal/mcpserver/`
Expected: the new key assertions fail.

**Step 3: Implement**

- `keyNames` adds `"CAPSOLVER_API_KEY"`.
- In `internal/daemon/tools.go`:
  - Import `internal/turnstile`.
  - `ActionReply` adds `Turnstile *turnstile.Status \`json:"turnstile,omitempty"\``.
  - Action tail: `if err != nil { return nil, err }; ts := p.EnsureTurnstile(ctx); return s.reply(..., ts)`.
  - `reply` takes `ts *turnstile.Status`; sets `out.Turnstile` when detected. Do not set the snapshot's Turnstile here, to avoid duplicating the MCP line.
  - `snapshot` case: `ts := p.EnsureTurnstile(ctx)`, take the snapshot, set `snap.Turnstile = ts`, return `text` plus a `turnstile` key.
  - `eval` case: ensure, return `map[string]any{"value": json.RawMessage(orNull(v)), "turnstile": ts}`.
- In `internal/mcpserver/server.go`:
  - Add `forwardKeysOnce` with a fingerprint and call it from `call` for every method. Return its error only for `run`; otherwise ignore.
  - Keep the 6 minute tool timeout.
- `CAPSOLVER_API_KEY` now reaches the daemon even when it started before the key was configured.

**Step 4: Run**

`go test ./... && BROJEV_E2E=1 go test ./internal/mcpserver/`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/daemon internal/mcpserver
git commit -m "feat(turnstile): resolve on page actions and snapshots"
```

---

## Task 6: Jev loop hook and blocked outcome

**Files:**
- Modify: `internal/agent/agent.go`

**Step 1: Write the failing test**

Add an agent test with a fixture page that contains a Turnstile block widget and a goal that cannot proceed. The fake Jev server answers DONE. Expected: the outcome contains a `turnstile` status and, for an interstitial-only page, `blocked`. If the fake Jev test is awkward, rely on the browser E2E plus a manual run and keep the existing loop tests green.

At minimum, add to `internal/agent/agent_test.go` a compile-time assertion that `Outcome` has `Turnstile *turnstile.Status` and a test that an ordinary page still reports `absent` and completes. Run it and watch it fail.

**Step 2: Implement**

- Import `internal/turnstile`.
- `Outcome` adds `Turnstile *turnstile.Status \`json:"turnstile,omitempty"\``.
- In `finish`, when taking the final snapshot, set `s.Turnstile = out.Turnstile`.
- In the loop, before `p.Snapshot`:
  - `ts := p.EnsureTurnstile(ctx)`
  - `snap.Turnstile = ts`
  - `if ts != nil && ts.Detected { out.Turnstile = ts }`
- After the snapshot:
  - Interstitial: `return finish(p, "blocked", "turnstile unsolved: full-page Cloudflare challenge")`
  - Unresolved with no usable content (`len(snap.Elements) == 0 && strings.TrimSpace(snap.Text) == ""`): `return finish(p, "blocked", "turnstile unsolved: "+ts.Reason)`
- Otherwise continue; Jev sees the page and can still choose BLOCKED.

**Step 3: Run**

`go test ./internal/agent/ && BROJEV_E2E=1 go test ./internal/agent/`
Expected: PASS.

**Step 4: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_test.go
git commit -m "feat(turnstile): resolve inside the Jev loop"
```

---

## Task 7: MCP rendering

**Files:**
- Modify: `internal/mcpserver/server.go`

**Step 1: Write the failing test**

Add a rendering test in `internal/mcpserver` (pure function, no browser):

```go
func TestRenderTurnstileLine(t *testing.T) {
	raw := []byte(`{"ms":1,"turnstile":{"detected":true,"state":"solved"}}`)
	out := renderAction("click", raw)
	if !strings.Contains(out, "TURNSTILE: solved") {
		t.Fatalf("render:\n%s", out)
	}
}
```

Run: `go test ./internal/mcpserver/ -run TestRenderTurnstileLine -v`
Expected: FAIL, no line.

**Step 2: Implement**

- Import `internal/turnstile`.
- `renderAction`'s anonymous struct adds `Turnstile *turnstile.Status \`json:"turnstile"\``.
- After the note and new-tab lines, write `r.Turnstile.Line()` when non-empty.
- `runTool`: add `Turnstile *turnstile.Status` to its anonymous struct and print the line after the status header. The final snapshot also carries the line; that is acceptable for the run summary.
- `snapshotTool` needs no change: the daemon text already includes the line.

**Step 3: Run**

`go test ./internal/mcpserver/ -run TestRenderTurnstileLine -v`
Expected: PASS.

**Step 4: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/server_test.go
git commit -m "feat(turnstile): render widget status in tool replies"
```

---

## Task 8: Config, doctor and docs

**Files:**
- Modify: `.claude-plugin/plugin.json`
- Modify: `.mcp.json`
- Modify: `cmd/brojev/main.go`
- Modify: `README.md`
- Modify: `skills/brojev/SKILL.md`
- Modify: `docs/ARCHITECTURE.md`
- Modify: `docs/plans/2026-11-20-turnstile-design.md` (correct the CapSolver `metadata` shape)

**Step 1: Config**

- `plugin.json` userConfig adds a sensitive `capsolver_api_key` with title `CapSolver API key (optional)` and a description covering the fallback, the cost and that empty disables it.
- `.mcp.json` adds `"BROJEV_SETTING_CAPSOLVER_API_KEY": "${user_config.capsolver_api_key}"`.
- `doctor()` calls `daemon.CleanKeyEnv()` and `daemon.LoadEnvFile()` before key checks, then prints either:
  - `[ok  ] capsolver configured (fallback, about $0.0012 per solve)`
  - `[ok  ] capsolver not configured (in-page only)`
  The key is never printed.

**Step 2: Docs**

- README: new `## Cloudflare Turnstile` section after the privacy section, covering behavior, in-page first, CapSolver fallback, `CAPSOLVER_API_KEY`, `TURNSTILE_TIMEOUT_MS`, cost, limits, and the status line.
- Privacy table: add a CapSolver row (page URL, sitekey, action, cdata).
- SKILL.md: replace the captcha bullet with automatic Turnstile handling, the status to expect, and a no-retry-loop rule. Other captchas still go to the user.
- ARCHITECTURE.md: replace `Captchas: hand to the human, no solver services` with the new behavior.
- Design doc: change the CapSolver request example to put `action` and `cdata` under `metadata`; note the list price and that keys are optional.

**Step 3: Run**

`go test ./...`
Expected: PASS.

**Step 4: Commit**

```bash
git add .claude-plugin/plugin.json .mcp.json cmd/brojev/main.go README.md skills/brojev/SKILL.md docs/ARCHITECTURE.md docs/plans/2026-11-20-turnstile-design.md
git commit -m "docs+config: Turnstile solver key and behavior"
```

---

## Task 9: Full regression and manual check

**Step 1: Unit tests**

```bash
go test ./...
```

**Step 2: Browser suites**

```bash
BROJEV_E2E=1 go test ./internal/page/ ./internal/mcpserver/ ./internal/agent/
```

The Turnstile fixtures need `challenges.cloudflare.com`. If headless is rejected, rerun with `BROJEV_HEADED=1`.

**Step 3: Manual real-page check**

Build and run against a page that embeds a Turnstile test widget, or open one of the fixtures through the normal daemon:

```bash
go build -o build/brojev ./cmd/brojev
./build/brojev open 'data:text/html,...'
```

Verify:
- `snapshot` shows `TURNSTILE: unresolved` while blocked, then `TURNSTILE: solved` after `EnsureTurnstile` runs.
- With no `CAPSOLVER_API_KEY`, resolution attempts in-page only and reports the reason.
- With a key set, a blocking widget still reports `solverUsed`.

**Step 4: Review the diff**

```bash
git diff --stat origin/main
git diff origin/main -- internal/turnstile internal/page/turnstile.go internal/agent/agent.go internal/daemon/tools.go internal/mcpserver/server.go
```

Check for: no key logging, no key in error strings, no duplicate status lines in replies, no hook on dialog-open pages.

**Step 5: Commit any fixes and push**

```bash
git add -A
git commit -m "test(turnstile): regression fixes"
git push
```

---

## Out of scope

- Solving full-page Cloudflare interstitials.
- Captcha types other than Turnstile.
- Fingerprint spoofing or stealth patches.
- A new MCP tool or a manual solve action.
- Hourly solver quotas.
