package daemon

// BROJEV_CDP_URL against a real Chrome the test owns. Run:
// BROJEV_E2E=1 go test ./internal/daemon/ -run CDPURL -v
//
// The Chrome here is headless, and a headless Chrome never exits when its
// last tab closes, so this test cannot reproduce the headful case the guard
// tab exists for (a headful Chrome on Linux/Windows quits with its last
// window; no Xvfb on the dev Mac). What it does prove is the mechanism: the
// guard tab survives closing every other tab, concurrently, and is recreated
// or adopted as needed.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
)

// startChrome runs headless Chrome on dir with the given debug port (0 picks
// one) and returns the process and its port.
func startChrome(t *testing.T, bin, dir, port string) (*exec.Cmd, string) {
	t.Helper()
	os.Remove(filepath.Join(dir, "DevToolsActivePort"))
	cmd := exec.Command(bin, "--headless=new", "--user-data-dir="+dir, "--remote-debugging-port="+port,
		"--no-first-run", "--no-default-browser-check", "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if port != "0" { // a fixed port writes no DevToolsActivePort
			if _, err := chrome.EndpointFromURL(context.Background(), "http://127.0.0.1:"+port); err == nil {
				return cmd, port
			}
		} else if b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort")); err == nil {
			if p, _, ok := strings.Cut(string(b), "\n"); ok && p != "" {
				return cmd, strings.TrimSpace(p)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	t.Fatal("chrome did not start its DevTools server")
	return nil, ""
}

type daemonProc struct {
	log  *bytes.Buffer
	done chan error
}

func startDaemon(t *testing.T, ctx context.Context, bin string) *daemonProc {
	t.Helper()
	d := &daemonProc{log: &bytes.Buffer{}, done: make(chan error, 1)}
	cmd := exec.Command(bin, "daemon")
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = d.log, d.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	for i := 0; i < 50 && Call(ctx, "ping", nil, nil) != nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return d
}

func (d *daemonProc) shutdown(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := Call(ctx, "shutdown", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit")
	}
}

// guards lists the guard tabs straight from Chrome's /json/list.
func guards(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/json/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []struct{ ID, Type, URL string }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range list {
		if p.Type == "page" && p.URL == browser.GuardURL {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

func TestCDPURLEndToEnd(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	home, prof := t.TempDir(), t.TempDir()
	bin := filepath.Join(home, "brojev")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/brojev").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	chromeBin, err := chrome.Find()
	if err != nil {
		t.Fatal(err)
	}
	chromeCmd, port := startChrome(t, chromeBin, prof, "0")
	t.Cleanup(func() { chromeCmd.Process.Kill(); chromeCmd.Wait() })
	base := "http://127.0.0.1:" + port

	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_CDP_URL", base)
	t.Setenv("BROJEV_CHROME", "/nonexistent/chrome") // proves brojev never launches one
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := startDaemon(t, ctx, bin)

	open := func(title string) {
		t.Helper()
		var info browser.PageInfo
		u := "data:text/html," + url.PathEscape("<title>"+title+"</title><p>hi")
		if err := Call(ctx, "open", map[string]string{"url": u}, &info); err != nil {
			t.Fatalf("open: %v\n%s", err, d.log.String())
		}
		if info.Title != title {
			t.Fatalf("title %q, want %q", info.Title, title)
		}
	}
	listTabs := func() []TabInfo {
		t.Helper()
		var tabs []TabInfo
		if err := Call(ctx, "tabs", nil, &tabs); err != nil {
			t.Fatal(err)
		}
		return tabs
	}
	oneGuard := func() string {
		t.Helper()
		g := guards(t, base)
		if len(g) != 1 {
			t.Fatalf("want exactly one guard tab, got %v", g)
		}
		return g[0]
	}
	statusGuard := func() string {
		t.Helper()
		var st struct {
			GuardTab string `json:"guardTab"`
		}
		if err := Call(ctx, "status", nil, &st); err != nil || st.GuardTab == "" {
			t.Fatalf("status guardTab: %q %v", st.GuardTab, err)
		}
		return st.GuardTab
	}
	const recheck = 2100 * time.Millisecond // EnsureGuard skips a guard confirmed < 2 s ago

	for _, title := range []string{"cdp one", "cdp two", "cdp three", "cdp four"} {
		open(title)
	}
	if err := Call(ctx, "profile", map[string]any{"action": "use", "name": "default", "quit": true}, nil); err == nil || err.Error() != errExternalChrome.Error() {
		t.Fatalf("profile use: %v", err)
	}
	guard := oneGuard()
	if statusGuard() != guard {
		t.Fatal("status reports another guard")
	}

	// A page that navigates itself to the marker URL is neither adopted nor closed.
	var imp browser.PageInfo
	if err := Call(ctx, "tab_new", map[string]string{"url": "data:text/html,<title>impostor</title>"}, &imp); err != nil {
		t.Fatal(err)
	}
	// A refused tab_close on the guard leaves the driven tab alone.
	if err := Call(ctx, "tab_close", map[string]string{"targetId": guard}, nil); err == nil {
		t.Fatal("tab_close on the guard succeeded")
	}
	for _, tab := range listTabs() {
		if tab.TargetID == imp.TargetID && !tab.Current {
			t.Fatal("refused tab_close cleared the current tab")
		}
	}
	if err := Call(ctx, "eval", map[string]string{"js": "location.href = '" + browser.GuardURL + "'"}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40 && len(guards(t, base)) < 2; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(recheck)
	listed := false
	for _, tab := range listTabs() {
		listed = listed || tab.TargetID == imp.TargetID
	}
	if !listed || len(guards(t, base)) != 2 || statusGuard() != guard {
		t.Fatalf("impostor: listed=%v guards=%v status guard=%s want %s", listed, guards(t, base), statusGuard(), guard)
	}
	if err := Call(ctx, "close", map[string]string{"targetId": imp.TargetID}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40 && len(guards(t, base)) > 1; i++ { // closeTarget is asynchronous
		time.Sleep(50 * time.Millisecond)
	}

	tabs := listTabs()
	for _, tab := range tabs {
		if tab.TargetID == guard {
			t.Fatal("tabs lists the guard tab")
		}
	}
	for _, m := range []string{"close", "tab_close", "tab_switch"} {
		if err := Call(ctx, m, map[string]string{"targetId": guard}, nil); err == nil || !strings.Contains(err.Error(), "guard tab") {
			t.Fatalf("%s on the guard: %v", m, err)
		}
	}

	// Close every other tab from two goroutines at once.
	var wg sync.WaitGroup
	errs := make(chan error, len(tabs))
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < len(tabs); i += 2 {
				errs <- Call(ctx, "close", map[string]string{"targetId": tabs[i].TargetID}, nil)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40 && len(listTabs()) > 0; i++ { // Target.closeTarget is asynchronous
		time.Sleep(50 * time.Millisecond)
	}
	if left := listTabs(); len(left) != 0 {
		t.Fatalf("tabs left: %v", left)
	}
	if oneGuard() != guard || statusGuard() != guard {
		t.Fatal("guard tab changed")
	}

	// Someone else closes the guard: the next call recreates it.
	if resp, err := http.Get(base + "/json/close/" + guard); err == nil {
		resp.Body.Close()
	}
	for i := 0; i < 40 && len(guards(t, base)) > 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(recheck)
	listTabs()
	if g := oneGuard(); g == guard {
		t.Fatal("guard was not recreated")
	}

	// Chrome restarts on the same port: the next call re-resolves the endpoint.
	chromeCmd.Process.Kill()
	chromeCmd.Wait()
	chromeCmd, _ = startChrome(t, chromeBin, prof, port)
	open("cdp five")
	guard = oneGuard()
	d.shutdown(t, ctx)
	if n := strings.Count(d.log.String(), "attached to "+base+" (external Chrome)"); n != 2 {
		t.Fatalf("want 2 attach lines (initial + after restart), got %d:\n%s", n, d.log.String())
	}

	// A new daemon adopts the existing guard instead of adding one.
	d = startDaemon(t, ctx, bin)
	listTabs()
	if g := oneGuard(); g != guard {
		t.Fatalf("guard %s not adopted (now %s)", guard, g)
	}
	d.shutdown(t, ctx)

	time.Sleep(300 * time.Millisecond)
	if err := chromeCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("chrome died with the daemon: %v", err)
	}
	if _, err := chrome.EndpointFromURL(ctx, base); err != nil {
		t.Fatalf("chrome stopped answering: %v", err)
	}
}
