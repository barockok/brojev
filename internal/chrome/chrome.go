// Package chrome finds, launches and reattaches to brojev's dedicated Chrome.
package chrome

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Find returns the Chrome binary: $BROJEV_CHROME, then well-known locations.
func Find() (string, error) {
	if p := os.Getenv("BROJEV_CHROME"); p != "" {
		return p, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("chrome not found; set BROJEV_CHROME")
}

// ActiveEndpoint reads DevToolsActivePort in profile (line 1 port, line 2
// path) and returns the browser WebSocket URL if something is listening.
func ActiveEndpoint(profile string) (string, error) {
	b, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("malformed DevToolsActivePort: %q", b)
	}
	port, path := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 300*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("stale DevToolsActivePort (port %s): %w", port, err)
	}
	conn.Close()
	return "ws://127.0.0.1:" + port + path, nil
}

type Options struct {
	Profile  string
	Headless bool
	Log      string // file for Chrome's stdout/stderr; empty discards
	// Attach connects through Chrome's own remote-debugging toggle
	// (chrome://inspect/#remote-debugging) instead of launching with
	// --remote-debugging-port, which on Chrome 154 makes every page report
	// navigator.webdriver=true. Chrome asks the user to allow each new
	// connection; brojevd holds one connection, so that is once per start.
	Attach bool
	// ProfileDirectory picks a person inside Profile ("Default", "Profile 3").
	ProfileDirectory string
}

// Ensure returns the endpoint of a Chrome running on Profile, launching one
// if needed. Chrome is started in its own process group so it outlives the
// daemon, and a restarted daemon reattaches through DevToolsActivePort.
func Ensure(ctx context.Context, o Options) (endpoint string, launched bool, err error) {
	if ep, err := ActiveEndpoint(o.Profile); err == nil {
		return ep, false, nil
	}
	bin, err := Find()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(o.Profile, 0o700); err != nil {
		return "", false, err
	}
	_ = os.Remove(filepath.Join(o.Profile, "DevToolsActivePort"))

	args := []string{"--user-data-dir=" + o.Profile}
	if o.ProfileDirectory != "" {
		args = append(args, "--profile-directory="+o.ProfileDirectory)
	}
	if !o.Attach {
		args = append(args, "--remote-debugging-port=0") // allowed because this is not the default profile
	}
	args = append(args,
		"--no-first-run",
		"--no-default-browser-check",
		// Our tabs keep rendering when the window is behind other apps.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		// Deliberately no --enable-automation: it sets navigator.webdriver.
	)
	if o.Headless {
		args = append(args, "--headless=new", "--window-size=1280,900")
	}
	args = append(args, "about:blank")

	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if o.Log != "" {
		f, err := os.OpenFile(o.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return "", false, err
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		return "", false, fmt.Errorf("start chrome: %w", err)
	}
	go cmd.Wait() // reap; Chrome may also hand off to an existing instance and exit

	wait := 20 * time.Second
	if o.Attach {
		wait = 2 * time.Minute
		fmt.Fprintln(os.Stderr, "brojev: in the Chrome window that opened, go to chrome://inspect/#remote-debugging and turn on remote debugging")
	}
	deadline := time.Now().Add(wait)
	for {
		if ep, err := ActiveEndpoint(o.Profile); err == nil {
			return ep, true, nil
		}
		if time.Now().After(deadline) {
			if o.Attach {
				return "", false, fmt.Errorf("remote debugging not enabled within %s: open chrome://inspect/#remote-debugging in the brojev Chrome and turn it on", wait)
			}
			return "", false, fmt.Errorf("chrome did not expose DevToolsActivePort in %s within %s (see %s)", o.Profile, wait, o.Log)
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
