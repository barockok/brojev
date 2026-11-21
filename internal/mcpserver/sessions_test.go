package mcpserver

// Several sessions on one Chrome. Run: BROJEV_E2E=1 go test -run Sessions ./internal/mcpserver/

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/barockok/brojev/internal/daemon"
)

func startTestDaemon(t *testing.T) context.Context {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "brojev")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/brojev").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_HEADLESS", "1")
	t.Setenv("BROJEV_SESSION_GRACE", "300ms")
	cmd := exec.Command(bin, "daemon", "--headless")
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(func() {
		cancel()
		daemon.Call(context.Background(), "shutdown", nil, nil)
		cmd.Wait()
		exec.Command("pkill", "-f", "user-data-dir="+home).Run()
		for i := 0; i < 50 && exec.Command("pgrep", "-f", "user-data-dir="+home).Run() == nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})
	for i := 0; i < 50 && daemon.Call(ctx, "ping", nil, nil) != nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return ctx
}

func TestSessionsDoNotCompete(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	ctx := startTestDaemon(t)

	ct, st := mcp.NewInMemoryTransports()
	go New().Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A tool call as one named session (two subagents of this Claude session,
	// or two Claude sessions: the daemon cannot tell the difference).
	call := func(sess, name string, args map[string]any) (string, error) {
		args["session"] = sess
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		if res.IsError {
			return b.String(), fmt.Errorf("%s: %s", name, b.String())
		}
		return b.String(), nil
	}
	ref := func(snap, needle string) string {
		for _, l := range strings.Split(snap, "\n") {
			if strings.Contains(l, needle) && strings.HasPrefix(l, "[") {
				return strings.Trim(strings.Fields(l)[0], "[]")
			}
		}
		t.Fatalf("no %q in\n%s", needle, snap)
		return ""
	}
	pageFor := func(who string) string {
		return "data:text/html," + url.PathEscape(`<title>`+who+`</title><input aria-label="Box"><button onclick="out.textContent='`+who+` got '+document.querySelector('input').value">Go</button><p id=out></p>
<a target=_blank href="data:text/html,popup-of-`+who+`">pop</a>`)
	}

	snapA, err := call("a", "navigate", map[string]any{"url": pageFor("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := call("b", "navigate", map[string]any{"url": pageFor("beta")})
	if err != nil {
		t.Fatal(err)
	}

	// Both drive their own page at the same time, interleaved.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	drive := func(sess, who, snap string) {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			word := fmt.Sprintf("%s-%d", who, i)
			if _, err := call(sess, "type", map[string]any{"ref": ref(snap, `"Box"`), "text": word, "snapshot": false}); err != nil {
				errs <- fmt.Errorf("%s type: %w", sess, err)
				return
			}
			if _, err := call(sess, "click", map[string]any{"ref": ref(snap, `"Go"`), "snapshot": false}); err != nil {
				errs <- fmt.Errorf("%s click: %w", sess, err)
				return
			}
			out, err := call(sess, "eval", map[string]any{"js": "out.textContent + '|' + document.title"})
			if err != nil {
				errs <- fmt.Errorf("%s eval: %w", sess, err)
				return
			}
			if want := who + " got " + word + "|" + who; !strings.Contains(out, want) {
				errs <- fmt.Errorf("session %s round %d: got %s, want %s", sess, i, out, want)
				return
			}
		}
	}
	wg.Add(2)
	go drive("a", "alpha", snapA)
	go drive("b", "beta", snapB)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Each session lists only its own tab.
	tabsA, _ := call("a", "tabs", map[string]any{})
	tabsB, _ := call("b", "tabs", map[string]any{})
	if !strings.Contains(tabsA, "alpha") || strings.Contains(tabsA, "beta") {
		t.Fatalf("a sees: %s", tabsA)
	}
	if !strings.Contains(tabsB, "beta") || strings.Contains(tabsB, "alpha") {
		t.Fatalf("b sees: %s", tabsB)
	}
	var listB []struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal([]byte(tabsB), &listB); err != nil || len(listB) != 1 {
		t.Fatalf("b tabs: %s (%v)", tabsB, err)
	}

	// Another session's tab is off limits unless asked for explicitly.
	if _, err := call("a", "tabs", map[string]any{"action": "switch", "targetId": listB[0].TargetID}); err == nil {
		t.Fatal("a switched to b's tab without shared")
	}
	if _, err := call("a", "tabs", map[string]any{"action": "close", "targetId": listB[0].TargetID}); err == nil {
		t.Fatal("a closed b's tab without shared")
	}

	// A popup belongs to the session that opened it.
	out, err := call("a", "click", map[string]any{"ref": ref(snapA, `"pop"`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "popup-of-alpha") {
		t.Fatalf("a did not see its popup:\n%s", out)
	}
	if tabsB2, _ := call("b", "tabs", map[string]any{}); strings.Contains(tabsB2, "popup-of-alpha") {
		t.Fatalf("b sees a's popup: %s", tabsB2)
	}
	// b still works on its own page afterwards.
	if out, err := call("b", "eval", map[string]any{"js": "document.title"}); err != nil || !strings.Contains(out, "beta") {
		t.Fatalf("b after a's popup: %q %v", out, err)
	}
}

func TestSessionLeaseCleansUp(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	ctx := startTestDaemon(t)
	tabCount := func() int {
		var st struct {
			Tabs int `json:"tabs"`
		}
		if err := daemon.Call(ctx, "status", nil, &st); err != nil {
			t.Fatal(err)
		}
		return st.Tabs
	}
	if err := daemon.CallAs(ctx, "other", "tab_new", map[string]any{"url": "about:blank"}, nil); err != nil {
		t.Fatal(err)
	}
	n0 := tabCount()

	lease, err := daemon.Lease(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.CallAs(ctx, "gone", "tab_new", map[string]any{"url": "about:blank"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := tabCount(); got != n0+1 {
		t.Fatalf("tabs after tab_new: %d, want %d", got, n0+1)
	}
	// Dropping the lease closes the session's window after the grace period...
	lease.Close()
	for i := 0; i < 40 && tabCount() != n0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if got := tabCount(); got != n0 {
		t.Fatalf("tabs after lease dropped: %d, want %d", got, n0)
	}
	// ...and changing the default profile leaves a live session's window alone.
	other, err := daemon.Lease(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := daemon.CallAs(ctx, "busy", "tab_new", map[string]any{"url": "about:blank"}, nil); err != nil {
		t.Fatal(err)
	}
	n1 := tabCount()
	if err := daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "add", "name": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "use", "name": "second"}, nil); err != nil {
		t.Fatalf("profile use while another session is live: %v", err)
	}
	var st struct {
		Tabs    int    `json:"tabs"`
		Profile string `json:"profile"`
	}
	if err := daemon.Call(ctx, "status", map[string]any{"profile": "default"}, &st); err != nil || st.Tabs != n1 {
		t.Fatalf("default profile after profile use: %+v %v, want %d tabs", st, err, n1)
	}
	// Closing the old default's Chrome is refused while that session works there.
	if err := daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "use", "name": "default"}, nil); err != nil {
		t.Fatal(err)
	}
	err = daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "use", "name": "second", "quit": true}, nil)
	if err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("profile use quit while another session is live: %v", err)
	}
}

// One agent, several profiles: every session is a window on its own profile,
// each profile is its own Chrome, and they run side by side.
func TestProfilesSideBySide(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	ctx := startTestDaemon(t)
	if err := daemon.Call(ctx, "profile", map[string]any{"action": "add", "name": "second"}, nil); err != nil {
		t.Fatal(err)
	}

	ct, st := mcp.NewInMemoryTransports()
	go New().Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) (string, error) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		if res.IsError {
			return b.String(), fmt.Errorf("%s", b.String())
		}
		return b.String(), nil
	}
	page := func(title string) string {
		return "data:text/html," + url.PathEscape("<title>"+title+"</title><h1>"+title+"</h1>")
	}

	// Two sessions on two profiles, started at the same time.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []map[string]any{
		{"session": "a", "url": page("page-a")},
		{"session": "b", "profile": "second", "url": page("page-b")},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = call("navigate", c)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("navigate %d: %v", i, err)
		}
	}
	// Each comes back to its own window, without repeating the profile.
	if out, err := call("snapshot", map[string]any{"session": "a"}); err != nil || !strings.Contains(out, "page-a") || strings.Contains(out, "page-b") {
		t.Fatalf("a: %q %v", out, err)
	}
	if out, err := call("snapshot", map[string]any{"session": "b"}); err != nil || !strings.Contains(out, "page-b") || strings.Contains(out, "page-a") {
		t.Fatalf("b: %q %v", out, err)
	}
	// A session cannot move to another profile.
	if out, err := call("snapshot", map[string]any{"session": "a", "profile": "second"}); err == nil || !strings.Contains(out, "cannot move") {
		t.Fatalf("a on second: %q %v", out, err)
	}
	// A third session on the second profile: a window of its own in that Chrome.
	if _, err := call("navigate", map[string]any{"session": "c", "profile": "second", "url": page("page-c")}); err != nil {
		t.Fatal(err)
	}
	if out, err := call("tabs", map[string]any{"session": "b"}); err != nil || !strings.Contains(out, "page-b") || strings.Contains(out, "page-c") {
		t.Fatalf("b tabs: %q %v", out, err)
	}

	// The two profiles are two Chromes.
	status := func(prof string) (st struct {
		Endpoint   string `json:"endpoint"`
		ProfileDir string `json:"profileDir"`
		Tabs       int    `json:"tabs"`
		Browsers   []struct {
			Profile  string `json:"profile"`
			Sessions int    `json:"sessions"`
		} `json:"browsers"`
	}) {
		if err := daemon.Call(ctx, "status", map[string]any{"profile": prof}, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	d, s2 := status("default"), status("second")
	if d.Endpoint == s2.Endpoint || d.ProfileDir == s2.ProfileDir {
		t.Fatalf("both profiles on one Chrome: %+v %+v", d, s2)
	}
	got := map[string]int{}
	for _, b := range s2.Browsers {
		got[b.Profile] = b.Sessions
	}
	if got["default"] != 1 || got["second"] != 2 {
		t.Fatalf("sessions per browser = %v, want default:1 second:2", got)
	}
	if out, err := call("profile", map[string]any{}); err != nil || !strings.Contains(out, `"sessions":2`) {
		t.Fatalf("profile list: %q %v", out, err)
	}
}

// A server pinned in its MCP config drives that one profile and nothing else.
func TestPinnedServer(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	ctx := startTestDaemon(t)
	if err := daemon.Call(ctx, "profile", map[string]any{"action": "add", "name": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PinEnv, "second")
	t.Cleanup(func() { pinned = "" })

	ct, st := mcp.NewInMemoryTransports()
	go New().Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) (string, bool) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String(), !res.IsError
	}
	page := "data:text/html," + url.PathEscape("<title>pinned</title><h1>pinned</h1>")

	// No profile given: the pin, not the default profile.
	if out, ok := call("navigate", map[string]any{"session": "a", "url": page}); !ok {
		t.Fatalf("navigate: %s", out)
	}
	var stt struct {
		Browsers []struct {
			Profile  string `json:"profile"`
			Sessions int    `json:"sessions"`
		} `json:"browsers"`
	}
	if err := daemon.Call(ctx, "status", map[string]any{"profile": "second"}, &stt); err != nil {
		t.Fatal(err)
	}
	if len(stt.Browsers) != 1 || stt.Browsers[0].Profile != "second" || stt.Browsers[0].Sessions != 1 {
		t.Fatalf("browsers = %+v, want only second with 1 session", stt.Browsers)
	}
	// Another profile cannot be chosen, and the default cannot be changed.
	for name, args := range map[string]map[string]any{
		"navigate": {"session": "b", "profile": "default", "url": page},
		"profile":  {"action": "use", "name": "default"},
	} {
		if out, ok := call(name, args); ok || !strings.Contains(out, "pinned") {
			t.Fatalf("%s past the pin: ok=%v %q", name, ok, out)
		}
	}
	if out, ok := call("profile", map[string]any{"action": "current"}); !ok || !strings.Contains(out, `"name":"second"`) {
		t.Fatalf("profile current: ok=%v %q", ok, out)
	}
}
