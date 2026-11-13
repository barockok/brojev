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
	// ...but a profile switch is refused while someone else holds a lease.
	other, err := daemon.Lease(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "add", "name": "second"}, nil); err != nil {
		t.Fatal(err)
	}
	err = daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "use", "name": "second"}, nil)
	if err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("profile use while another session is live: %v", err)
	}
	if err := daemon.CallAs(ctx, "me", "profile", map[string]any{"action": "use", "name": "second", "force": true}, nil); err != nil {
		t.Fatalf("forced profile use: %v", err)
	}
}
