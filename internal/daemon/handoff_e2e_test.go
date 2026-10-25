package daemon

// Handoff end to end: the agent drives one Chrome (BROJEV_CDP_URL), a
// "person" logs in in another (BROJEV_HUMAN_CDP_URL), and the agent continues
// logged in. Run: BROJEV_E2E=1 go test ./internal/daemon/ -run Handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
)

// loginSite needs a person: /login only accepts a form with the "human" box
// ticked, then sets an HttpOnly session cookie and a localStorage key.
func loginSite() *httptest.Server {
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, title, body string) {
		fmt.Fprintf(w, "<html><head><title>%s</title></head><body>%s</body></html>", title, body)
	}
	cookie := func(r *http.Request, name string) string {
		if c, err := r.Cookie(name); err == nil {
			return c.Value
		}
		return ""
	}
	mux.HandleFunc("/addcart", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "cart", Value: "3", Path: "/"})
		page(w, "Cart", "<p>cart has 3 items</p>")
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.FormValue("human") != "on" {
				w.WriteHeader(http.StatusForbidden)
				page(w, "Login", "prove you are human")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s-" + r.FormValue("user"), Path: "/", HttpOnly: true})
			page(w, "Welcome", "<p>logged in</p><script>localStorage.setItem('pref','dark')</script>")
			return
		}
		page(w, "Login", "<p>cart="+cookie(r, "cart")+`</p><form method=post action=/login>
<input name=user id=user><label><input type=checkbox name=human id=human> I am human</label><button id=go>Log in</button></form>`)
	})
	mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
		sid := cookie(r, "sid")
		if sid == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		page(w, "Account", "<h1>Account of "+strings.TrimPrefix(sid, "s-")+"</h1><p>cart="+cookie(r, "cart")+
			"</p><p id=pref></p><script>document.getElementById('pref').textContent='pref='+localStorage.getItem('pref')</script>")
	})
	return httptest.NewServer(mux)
}

func TestHandoffEndToEnd(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "brojev")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/brojev").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	chromeBin, err := chrome.Find()
	if err != nil {
		t.Fatal(err)
	}
	agentCmd, agentPort := startChrome(t, chromeBin, t.TempDir(), "0")
	t.Cleanup(func() { agentCmd.Process.Kill(); agentCmd.Wait() })
	humanCmd, humanPort := startChrome(t, chromeBin, t.TempDir(), "0")
	t.Cleanup(func() { humanCmd.Process.Kill(); humanCmd.Wait() })
	humanBase := "http://127.0.0.1:" + humanPort
	site := loginSite()
	defer site.Close()

	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_CDP_URL", "http://127.0.0.1:"+agentPort)
	t.Setenv("BROJEV_HUMAN_CDP_URL", humanBase)
	t.Setenv("BROJEV_HUMAN_VIEW_URL", "http://view.test/vnc.html")
	t.Setenv("BROJEV_CHROME", "/nonexistent/chrome")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := startDaemon(t, ctx, bin)
	defer d.shutdown(t, ctx)

	eval := func(js string) string {
		t.Helper()
		var out struct {
			Value json.RawMessage `json:"value"`
		}
		if err := Call(ctx, "eval", map[string]string{"js": js}, &out); err != nil {
			t.Fatalf("eval %s: %v\n%s", js, err, d.log.String())
		}
		return strings.Trim(string(out.Value), `"`)
	}
	for _, path := range []string{"/addcart", "/account"} {
		if err := Call(ctx, "navigate", map[string]string{"url": site.URL + path}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := eval("document.title"); got != "Login" {
		t.Fatalf("agent not at the login wall: %q", got)
	}

	var started struct {
		Status  string      `json:"status"`
		URL     string      `json:"url"`
		ViewURL string      `json:"viewUrl"`
		Sent    transferred `json:"sent"`
	}
	if err := Call(ctx, "handoff", map[string]string{"action": "start", "reason": "log in"}, &started); err != nil {
		t.Fatalf("handoff start: %v\n%s", err, d.log.String())
	}
	if started.Status != "waiting_for_person" || started.URL != site.URL+"/login" || started.ViewURL != "http://view.test/vnc.html" || started.Sent.Cookies < 1 {
		t.Fatalf("handoff start: %+v", started)
	}
	if err := Call(ctx, "handoff", map[string]string{"action": "start"}, nil); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("second start: %v", err)
	}

	// The person, in their own Chrome: sees the agent's cart, logs in.
	ep, err := chrome.EndpointFromURL(ctx, humanBase)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := browser.Connect(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	defer hb.Close()
	pages, err := hb.Pages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tab *browser.Tab
	for _, p := range pages {
		if p.URL == site.URL+"/login" {
			if tab, err = hb.Attach(ctx, p.TargetID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tab == nil {
		t.Fatalf("no handoff tab in the person's Chrome: %v", pages)
	}
	var body string
	if err := tab.Eval(ctx, "document.body.innerText", &body); err != nil || !strings.Contains(body, "cart=3") {
		t.Fatalf("person's page lacks the agent's cookie: %q %v", body, err)
	}
	if err := tab.Eval(ctx, `user.value='alice'; human.checked=true; go.click(); true`, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		var title string
		if tab.Eval(ctx, "document.title", &title) == nil && title == "Welcome" {
			break
		}
		if i > 50 {
			t.Fatalf("person's login did not finish (title %q)", title)
		}
		time.Sleep(100 * time.Millisecond)
	}

	var finished struct {
		Status   string      `json:"status"`
		Received transferred `json:"received"`
		Page     struct {
			URL  string `json:"url"`
			Note string `json:"note"`
		} `json:"page"`
	}
	if err := Call(ctx, "handoff", map[string]string{"action": "finish", "url": site.URL + "/account"}, &finished); err != nil {
		t.Fatalf("handoff finish: %v\n%s", err, d.log.String())
	}
	if finished.Status != "finished" || finished.Received.Cookies < 2 || finished.Received.LocalStorage != 1 || finished.Page.URL != site.URL+"/account" {
		t.Fatalf("handoff finish: %+v", finished)
	}
	if got := eval("document.body.innerText"); !strings.Contains(got, "Account of alice") || !strings.Contains(got, "cart=3") || !strings.Contains(got, "pref=dark") {
		t.Fatalf("agent not logged in after handoff: %q", got)
	}
	var st struct {
		Open bool `json:"open"`
	}
	if err := Call(ctx, "handoff", map[string]string{"action": "status"}, &st); err != nil || st.Open {
		t.Fatalf("status after finish: %+v %v", st, err)
	}
	if pages, _ := hb.Pages(ctx); len(pages) != 1 {
		t.Fatalf("handoff tab not closed in the person's Chrome: %d tabs", len(pages))
	}
}
