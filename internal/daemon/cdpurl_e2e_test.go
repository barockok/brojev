package daemon

// BROJEV_CDP_URL against a real Chrome the test owns. Run:
// BROJEV_E2E=1 go test ./internal/daemon/ -run CDPURL -v

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	var dlog bytes.Buffer
	d := exec.Command(bin, "daemon")
	d.Env = os.Environ()
	d.Stdout, d.Stderr = &dlog, &dlog
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Wait() }()
	t.Cleanup(func() { d.Process.Kill(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := 0; i < 50 && Call(ctx, "ping", nil, nil) != nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	open := func(title string) {
		t.Helper()
		var info browser.PageInfo
		u := "data:text/html," + url.PathEscape("<title>"+title+"</title><p>hi")
		if err := Call(ctx, "open", map[string]string{"url": u}, &info); err != nil {
			t.Fatalf("open: %v\n%s", err, dlog.String())
		}
		if info.Title != title {
			t.Fatalf("title %q, want %q", info.Title, title)
		}
	}
	open("cdp one")
	if err := Call(ctx, "profile", map[string]any{"action": "use", "name": "default", "quit": true}, nil); err == nil || err.Error() != errExternalChrome.Error() {
		t.Fatalf("profile use: %v", err)
	}

	// Closing every tab must leave Chrome running (the last one is replaced).
	var tabs []TabInfo
	if err := Call(ctx, "tabs", nil, &tabs); err != nil || len(tabs) < 2 {
		t.Fatalf("tabs: %v %v", tabs, err)
	}
	for _, tab := range tabs {
		if err := Call(ctx, "close", map[string]string{"targetId": tab.TargetID}, nil); err != nil {
			t.Fatalf("close %s: %v", tab.TargetID, err)
		}
	}
	if err := Call(ctx, "tabs", nil, &tabs); err != nil || len(tabs) != 1 {
		t.Fatalf("after closing all: %v %v", tabs, err)
	}

	// Chrome restarts on the same port: the next call re-resolves the endpoint.
	chromeCmd.Process.Kill()
	chromeCmd.Wait()
	chromeCmd, _ = startChrome(t, chromeBin, prof, port)
	open("cdp two")

	if err := Call(ctx, "shutdown", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		done <- nil // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit")
	}
	time.Sleep(300 * time.Millisecond)
	if err := chromeCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("chrome died with the daemon: %v", err)
	}
	if _, err := chrome.EndpointFromURL(ctx, base); err != nil {
		t.Fatalf("chrome stopped answering: %v", err)
	}
	if n := strings.Count(dlog.String(), "attached to "+base+" (external Chrome)"); n != 2 {
		t.Fatalf("want 2 attach lines (initial + after restart), got %d:\n%s", n, dlog.String())
	}
}
