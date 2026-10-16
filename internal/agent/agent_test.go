package agent

// Loop tests: real headless Chrome on local fixtures, fake Jev server that
// answers like the real API. No paid calls. Run: BROJEV_E2E=1 go test ./internal/agent/

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/jev"
	"github.com/barockok/brojev/internal/llm"
	"github.com/barockok/brojev/internal/page"
)

const formHTML = `<!doctype html><title>signup</title>
<form onsubmit="event.preventDefault(); document.title='welcome'; out.textContent='Welcome, '+document.querySelector('[name=n]').value">
<label>Name <input name=n></label> <label>Plan <select name=plan><option>Free</option><option>Pro</option></select></label>
<button>Create account</button></form><p id=out></p>
<button onclick="window.open('/popup')">Open help</button>`

var (
	base string
	brw  *browser.Browser
)

var e2e = os.Getenv("BROJEV_E2E") == "1"

func TestMain(m *testing.M) {
	if !e2e {
		fmt.Println("skipping browser tests; set BROJEV_E2E=1")
		os.Exit(m.Run())
	}
	dir, _ := os.MkdirTemp("", "brojev-agent-")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/slow" {
			// A shell that renders the real form only after 1.2 s, like an SPA boot.
			fmt.Fprint(w, `<!doctype html><title>boot</title><p>Loading...</p><script>setTimeout(()=>{document.body.innerHTML=`+"`"+`<form onsubmit="event.preventDefault(); out.textContent='Welcome, '+n.value"><label>Name <input id=n name=n></label><button>Create account</button></form><p id=out></p>`+"`"+`},1200)</script>`)
			return
		}
		if r.URL.Path == "/noop" {
			fmt.Fprint(w, `<!doctype html><title>noop</title><button>Do nothing</button> <button>Other</button>`)
			return
		}
		if r.URL.Path == "/big" {
			var b strings.Builder
			b.WriteString("<!doctype html><title>big</title>")
			for i := 0; i < 300; i++ {
				fmt.Fprintf(&b, "<div><button>item %d</button></div>", i)
			}
			for i := 1; i <= 4; i++ {
				fmt.Fprintf(&b, "<div><button onclick=\"document.title='archived %d'\">Archive thread %d</button></div>", i, i)
			}
			fmt.Fprint(w, b.String())
			return
		}
		if r.URL.Path == "/popup" {
			fmt.Fprint(w, `<!doctype html><title>help</title><p>help center</p>`)
			return
		}
		fmt.Fprint(w, formHTML)
	}))
	base = "http://" + ln.Addr().String()
	ctx := context.Background()
	ep, _, err := chrome.Ensure(ctx, chrome.Options{Profile: filepath.Join(dir, "p"), Headless: true})
	if err != nil {
		panic(err)
	}
	brw, err = browser.Connect(ctx, ep)
	if err != nil {
		panic(err)
	}
	code := m.Run()
	brw.Conn().Browser().Call(ctx, "Browser.close", nil, nil)
	time.Sleep(200 * time.Millisecond)
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is a minimal agent.Env over the shared browser.
type env struct {
	mu  sync.Mutex
	cur *page.Page
	all map[string]*page.Page
}

func (e *env) Current(ctx context.Context) (*page.Page, error) { return e.cur, nil }
func (e *env) TabIDs(ctx context.Context) ([]string, error) {
	ps, err := brw.Pages(ctx)
	var ids []string
	for _, p := range ps {
		ids = append(ids, string(p.TargetID))
	}
	return ids, err
}
func (e *env) Switch(ctx context.Context, id string) (*page.Page, error) {
	tab, err := brw.Attach(ctx, target.ID(id))
	if err != nil {
		return nil, err
	}
	p, err := page.New(ctx, tab.ID, tab.Session)
	if err != nil {
		return nil, err
	}
	e.cur = p
	return p, nil
}

func newEnv(t *testing.T) *env {
	if !e2e {
		t.Skip("set BROJEV_E2E=1")
	}
	ctx := context.Background()
	info, err := brw.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{}
	if _, err := e.Switch(ctx, info.TargetID); err != nil {
		t.Fatal(err)
	}
	return e
}

type req struct {
	State struct {
		Page struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"page"`
		Elements []map[string]any `json:"elements"`
	} `json:"state"`
	Questions map[string]struct {
		Criteria map[string]any `json:"criteria"`
	} `json:"questions"`
}

// fakeJev answers each request with decide(state) -> (op, target).
func fakeJev(t *testing.T, decide func(r *req) (string, string, float64)) (*jev.Client, *[]req) {
	var mu sync.Mutex
	var seen []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		if hr.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "auth", 401)
			return
		}
		b, _ := io.ReadAll(hr.Body)
		var r req
		if err := json.Unmarshal(b, &r); err != nil {
			t.Errorf("bad request: %v", err)
		}
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		op, tgt, p := decide(&r)
		answers := map[string]any{}
		for qid, q := range r.Questions {
			choice := ""
			for k := range q.Criteria {
				if choice == "" {
					choice = k
				}
			}
			if qid == "operation" {
				choice = op
			} else if tgt != "" {
				if _, ok := q.Criteria[tgt]; ok {
					choice = tgt
				}
			}
			probs := map[string]float64{}
			rest := (1 - p) / float64(max(len(q.Criteria)-1, 1))
			for k := range q.Criteria {
				probs[k] = rest
			}
			if len(q.Criteria) == 1 {
				probs[choice] = 1
			} else {
				probs[choice] = p
			}
			answers[qid] = map[string]any{"type": "choice", "choice": choice, "probabilities": probs, "confidence": p}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers, "usage": map[string]int{"input_tokens": 1000}})
	}))
	t.Cleanup(srv.Close)
	return &jev.Client{Key: "test", Model: "fake", Endpoint: srv.URL, HTTP: srv.Client()}, &seen
}

// index finds the element index whose label contains s.
func index(r *req, s string) string {
	for _, e := range r.State.Elements {
		if strings.Contains(e["label"].(string), s) {
			return e["index"].(string)
		}
	}
	return ""
}

func TestLoopFillsSelectsSubmits(t *testing.T) {
	e := newEnv(t)
	j, seen := fakeJev(t, func(r *req) (string, string, float64) {
		switch {
		case strings.Contains(r.State.Page.Text, "Welcome, Ada"):
			return "DONE", "", 0.9
		case !strings.Contains(fmt.Sprint(r.State.Elements), "value:Ada"):
			return "TYPE_TEXT", index(r, `"Name"`) + "|name", 0.9
		case !strings.Contains(fmt.Sprint(r.State.Elements), "value:Pro"):
			// options are "<index>:<n>"; Pro is the 2nd
			return "SELECT", index(r, "combobox") + ":2", 0.9
		default:
			return "CLICK", index(r, "Create account"), 0.9
		}
	})
	run := &Runner{Jev: j, Env: e}
	out, err := run.Run(context.Background(), Task{Goal: "Sign up as Ada on the Pro plan", URL: base, Values: map[string]string{"name": "Ada"}, Expect: "Welcome, Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" || out.Verified == nil || !*out.Verified {
		t.Fatalf("outcome: %+v\nsteps: %+v", out, out.Steps)
	}
	// The unambiguous Name/name pair is typed without a model call.
	if out.JevCalls != 2 || len(out.Steps) != 4 {
		t.Fatalf("want 2 decisions and 4 steps, got %d (%+v)", out.JevCalls, out.Steps)
	}
	if out.Steps[0].Text != "Ada" || out.Steps[0].Note != "prefill name" || out.Steps[1].Op != "SELECT" || !out.Steps[2].PageChanged {
		t.Fatalf("steps: %+v", out.Steps)
	}
	// Heads are restricted by construction.
	first := (*seen)[0]
	if _, ok := first.Questions["select_target"].Criteria[index(&first, "combobox")+":1"]; ok {
		t.Fatal("already-selected option must not be offered")
	}
	if out.CostUSD <= 0 || out.TotalMS <= 0 {
		t.Fatalf("accounting: %+v", out)
	}
}

func TestLowConfidenceStopsForClaude(t *testing.T) {
	e := newEnv(t)
	j, _ := fakeJev(t, func(r *req) (string, string, float64) { return "HOVER", index(r, "Create account"), 0.42 }) // joint 0.18 < 0.2, still argmax
	out, err := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "do something vague", URL: base})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs_claude" || !strings.Contains(out.Reason, "low confidence") {
		t.Fatalf("outcome: %s %s", out.Status, out.Reason)
	}
	if out.Snapshot == "" {
		t.Fatal("needs_claude must hand back the page")
	}
}

func TestNoProgressIsBlocked(t *testing.T) {
	e := newEnv(t)
	j, _ := fakeJev(t, func(r *req) (string, string, float64) { return "HOVER", index(r, "Create account"), 0.9 })
	out, _ := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "hover forever", URL: base})
	if out.Status != "blocked" || !strings.Contains(out.Reason, "no progress") || len(out.Steps) != 3 {
		t.Fatalf("outcome: %s %s steps=%d", out.Status, out.Reason, len(out.Steps))
	}
}

func TestFollowsNewTab(t *testing.T) {
	e := newEnv(t)
	j, _ := fakeJev(t, func(r *req) (string, string, float64) {
		if r.State.Page.Title == "help" {
			return "DONE", "", 0.9
		}
		return "CLICK", index(r, "Open help"), 0.9
	})
	out, _ := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "open the help center", URL: base})
	if out.Status != "done" || out.Title != "help" || !strings.Contains(out.Steps[0].Note, "switched to new tab") {
		t.Fatalf("outcome: %s %s title=%q steps=%+v", out.Status, out.Reason, out.Title, out.Steps)
	}
}

func TestMissingValueAsksClaude(t *testing.T) {
	e := newEnv(t)
	// No values, no helper: TYPE_TEXT is not even offered, so Jev must block.
	j, seen := fakeJev(t, func(r *req) (string, string, float64) { return "BLOCKED", "", 0.9 })
	out, _ := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "sign up", URL: base})
	if _, ok := (*seen)[0].Questions["type_text_target"]; ok {
		t.Fatal("TYPE_TEXT offered with no way to produce text")
	}
	if out.Status != "blocked" {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestWaitsForSlowShell(t *testing.T) {
	e := newEnv(t)
	j, seen := fakeJev(t, func(r *req) (string, string, float64) {
		switch {
		case strings.Contains(r.State.Page.Text, "Welcome, Ada"):
			return "DONE", "", 0.9
		case index(r, "Create account") == "":
			return "BLOCKED", "", 0.9 // would be a false BLOCKED on the unrendered shell
		case !strings.Contains(fmt.Sprint(r.State.Elements), "value:Ada"):
			return "TYPE_TEXT", index(r, `"Name"`) + "|name", 0.9
		default:
			return "CLICK", index(r, "Create account"), 0.9
		}
	})
	out, err := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "Sign up as Ada", URL: base + "/slow", Values: map[string]string{"name": "Ada"}, Expect: "Welcome, Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("outcome: %s %s steps=%+v", out.Status, out.Reason, out.Steps)
	}
	if first := (*seen)[0]; index(&first, "Create account") == "" {
		t.Fatal("first decision was made on the unrendered shell")
	}
}

// 304 buttons: the target is far below the fold and not in the default
// option list; the loop must reach it through FIND, then click.
func TestFindReachesHiddenElement(t *testing.T) {
	e := newEnv(t)
	j, seen := fakeJev(t, func(r *req) (string, string, float64) {
		if strings.HasPrefix(r.State.Page.Title, "archived") {
			return "DONE", "", 0.9
		}
		if q, ok := r.Questions["find_phrase"]; ok {
			for k, c := range q.Criteria {
				if m, ok := c.(map[string]any); ok && m["phrase"] == "archive" {
					return "FIND", k, 0.9
				}
			}
		}
		return "CLICK", index(r, "Archive thread 3"), 0.9
	})
	out, err := (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "Open the old conversation", URL: base + "/big"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" || out.Title != "archived 3" {
		t.Fatalf("outcome: %s %s title=%q steps=%+v", out.Status, out.Reason, out.Title, out.Steps)
	}
	if len(out.Steps) < 3 || out.Steps[0].Op != "FIND" || out.Steps[1].Op != "CLICK" {
		t.Fatalf("steps: %+v", out.Steps)
	}
	if first := (*seen)[0]; index(&first, "Archive thread 3") != "" {
		t.Fatal("target was offered in the default view")
	}
}

// The helper model proposes a phrase the code could not derive: the target
// ("Resolve") shares nothing with the goal, and the recurring-word heuristic
// cannot see it (it occurs once). Only the model's hint makes FIND reach it.
func TestHelperHintReachesUnnamedTarget(t *testing.T) {
	e := newEnv(t)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reply := `{"phrases": []}`
		if strings.Contains(string(b), "hidden_names") {
			reply = `{"phrases": ["thread 3"]}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}})
	}))
	defer hs.Close()
	j, _ := fakeJev(t, func(r *req) (string, string, float64) {
		if strings.HasPrefix(r.State.Page.Title, "archived") {
			return "DONE", "", 0.9
		}
		if q, ok := r.Questions["find_phrase"]; ok {
			for k, c := range q.Criteria {
				if m, ok := c.(map[string]any); ok && m["phrase"] == "thread 3" {
					return "FIND", k, 0.9
				}
			}
		}
		return "CLICK", index(r, "Archive thread 3"), 0.9
	})
	run := &Runner{Jev: j, Env: e, LLM: &llm.Client{Key: "k", BaseURL: hs.URL, Model: "m", HTTP: hs.Client()}}
	out, err := run.Run(context.Background(), Task{Goal: "Open the old conversation", URL: base + "/big"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" || out.Title != "archived 3" || out.Steps[0].Op != "FIND" || out.Steps[0].Target != "thread 3" {
		t.Fatalf("outcome: %s %s title=%q steps=%+v", out.Status, out.Reason, out.Title, out.Steps)
	}
	if out.HelperMS < 0 {
		t.Fatal("helper time not accounted")
	}
}

// A click that changes nothing is not offered again until the page changes, so
// Jev has to try something else.
func TestNoopClickIsNotOfferedAgain(t *testing.T) {
	e := newEnv(t)
	j, seen := fakeJev(t, func(r *req) (string, string, float64) {
		return "CLICK", index(r, "Do nothing"), 0.9
	})
	_, _ = (&Runner{Jev: j, Env: e}).Run(context.Background(), Task{Goal: "x", URL: base + "/noop", MaxSteps: 4})
	if len(*seen) < 2 {
		t.Fatalf("only %d decisions", len(*seen))
	}
	offered := func(q req, label string) bool {
		for _, c := range q.Questions["click_target"].Criteria {
			if m, ok := c.(map[string]any); ok && strings.Contains(fmt.Sprint(m["element"]), label) {
				return true
			}
		}
		return false
	}
	if !offered((*seen)[0], "Do nothing") {
		t.Fatal("setup: first decision should offer the button")
	}
	if offered((*seen)[1], "Do nothing") {
		t.Fatal("a click that changed nothing was offered again on the same page")
	}
	if !offered((*seen)[1], "Other") {
		t.Fatal("the other button must still be offered")
	}
}
