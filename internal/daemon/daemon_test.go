package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barockok/brojev/internal/version"
)

func TestCDPURLGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_CHROME", "/nonexistent/chrome") // a launch attempt would fail differently
	dead := httptest.NewServer(http.NotFoundHandler())
	base := dead.URL
	dead.Close()
	t.Setenv("BROJEV_CDP_URL", base)
	s := &Server{}
	ctx := context.Background()

	for _, quit := range []bool{false, true} {
		raw, _ := json.Marshal(ProfileParams{Action: "use", Name: "default", Quit: quit})
		if _, err := s.profileTool(ctx, raw); err == nil || err.Error() != errExternalChrome.Error() {
			t.Fatalf("profile use (quit=%v): err = %v, want %q", quit, err, errExternalChrome)
		}
	}
	// The connect path resolves the URL and never falls back to launching Chrome.
	_, err := s.dispatch(ctx, Request{Method: "status"})
	if err == nil || !strings.Contains(err.Error(), base) {
		t.Fatalf("status: err = %v, want it to name %s", err, base)
	}
	if _, err := os.Stat(filepath.Join(home, "profile")); !os.IsNotExist(err) {
		t.Fatalf("a Chrome profile dir was created: %v", err)
	}
}

// fakeDaemon answers ping with ver on the socket and records every method.
func fakeDaemon(t *testing.T, ver string) (methods func() []string) {
	t.Helper()
	ln, err := net.Listen("unix", SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(SocketPath()) })
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req Request
					json.Unmarshal(sc.Bytes(), &req)
					mu.Lock()
					seen = append(seen, req.Method)
					mu.Unlock()
					res, _ := json.Marshal(map[string]any{"pid": 1, "version": ver})
					json.NewEncoder(c).Encode(Response{Result: res})
				}
			}()
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestNoSpawnWithoutDaemon(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_NO_SPAWN", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "brojev daemon not running") {
		t.Fatalf("Ensure: err = %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(SocketPath()); !os.IsNotExist(err) {
		t.Fatalf("a daemon socket appeared: %v", err)
	}
}

func TestNoSpawnVersionMismatch(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_NO_SPAWN", "1")
	methods := fakeDaemon(t, "0.0.0-other")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "0.0.0-other") || !strings.Contains(err.Error(), version.Version) {
		t.Fatalf("Ensure: err = %v, want both versions", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := strings.Join(methods(), ","); got != "ping" {
		t.Fatalf("requests = %q, want only ping (no shutdown)", got)
	}
}

func TestNoSpawnSameVersion(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_NO_SPAWN", "1")
	fakeDaemon(t, version.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ensure(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNoSpawnValues(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "No": false, " OFF ": false, "1": true, "yes": true, "true": true} {
		t.Setenv("BROJEV_NO_SPAWN", v)
		if NoSpawn() != want {
			t.Fatalf("BROJEV_NO_SPAWN=%q: NoSpawn() = %v", v, !want)
		}
	}
}
