// Package daemon runs brojevd: it owns the single CDP connection (so Chrome
// asks for remote-debugging approval at most once) and serves short-lived
// clients (MCP server, CLI) over a unix socket with newline-delimited JSON.
package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
	"github.com/barockok/brojev/internal/chrome"
	"github.com/barockok/brojev/internal/jev"
	"github.com/barockok/brojev/internal/llm"
	"github.com/barockok/brojev/internal/page"
	"github.com/barockok/brojev/internal/profile"
)

// Home is brojev's state dir: $BROJEV_HOME or ~/.brojev.
func Home() string {
	if h := os.Getenv("BROJEV_HOME"); h != "" {
		return h
	}
	d, _ := os.UserHomeDir()
	return filepath.Join(d, ".brojev")
}

// SocketPath is the daemon socket. Unix socket paths are limited to 104 bytes
// on macOS, so a long Home falls back to a short per-home path in /tmp.
func SocketPath() string {
	p := filepath.Join(Home(), "brojevd.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(Home()))
	return filepath.Join("/tmp", fmt.Sprintf("brojevd-%d-%x.sock", os.Getuid(), sum[:6]))
}

// ActiveProfile is the Chrome profile brojev drives (see package profile).
func ActiveProfile() (profile.Profile, error) { return profile.Active(Home()) }
func LogPath() string                         { return filepath.Join(Home(), "brojevd.log") }
func chromeLog() string                       { return filepath.Join(Home(), "chrome.log") }

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Server struct {
	Headless bool

	mu       sync.Mutex
	browser  *browser.Browser
	prof     profile.Profile // profile of the live connection
	pages    map[target.ID]*page.Page
	cur      target.ID
	jev      *jev.Client
	llm      *llm.Client
	started  time.Time
	quit     chan struct{}
	quitOnce sync.Once
}

// Serve listens on the socket until shutdown is requested or ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	sock := SocketPath()
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		c.Close()
		return errors.New("brojevd already running at " + sock)
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	// Whoever can reach the socket can drive the browser: owner only.
	if err := os.Chmod(sock, 0o600); err != nil {
		return err
	}
	s.started = time.Now()
	s.quit = make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-s.quit:
		}
		ln.Close()
	}()
	log.Printf("brojevd listening on %s", sock)
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req Request
		var resp Response
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp.Error = "bad request: " + err.Error()
		} else if out, err := s.dispatch(ctx, req); err != nil {
			resp.Error = err.Error()
		} else if out != nil {
			b, err := json.Marshal(out)
			if err != nil {
				resp.Error = err.Error()
			}
			resp.Result = b
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// conn returns the live browser, (re)connecting and launching Chrome if needed.
func (s *Server) conn(ctx context.Context) (*browser.Browser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prof, err := ActiveProfile()
	if err != nil {
		return nil, err
	}
	if s.browser != nil {
		select {
		case <-s.browser.Done():
			s.dropLocked() // Chrome quit or the socket dropped
		default:
			if prof == s.prof {
				return s.browser, nil
			}
			log.Printf("profile changed %s -> %s; reconnecting", s.prof.Name, prof.Name)
			s.dropLocked()
		}
	}
	attach := prof.Attach
	ep, launched, err := chrome.Ensure(ctx, chrome.Options{Profile: prof.Dir, ProfileDirectory: prof.ChromeProfile, Headless: s.Headless, Log: chromeLog(), Attach: attach})
	if err != nil {
		return nil, err
	}
	dctx := ctx
	if attach { // the WebSocket handshake waits for the user's Allow click
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	b, err := browser.Connect(dctx, ep)
	if err != nil {
		return nil, err
	}
	log.Printf("connected to %s (profile=%s launched=%v)", ep, prof.Name, launched)
	s.prof = prof
	if err := b.EnableDownloads(ctx, filepath.Join(Home(), "downloads")); err != nil {
		log.Printf("downloads: %v", err)
	}
	s.pages, s.cur = map[target.ID]*page.Page{}, ""
	s.browser = b
	return b, nil
}

// dropLocked forgets the live connection (Chrome itself keeps running).
func (s *Server) dropLocked() {
	for _, p := range s.pages {
		p.Close()
	}
	s.pages, s.cur = map[target.ID]*page.Page{}, ""
	if s.browser != nil {
		s.browser.Close()
		s.browser = nil
	}
	s.prof = profile.Profile{}
}

func (s *Server) dispatch(ctx context.Context, req Request) (any, error) {
	switch req.Method {
	case "ping":
		return map[string]any{"pid": os.Getpid(), "uptimeSec": int(time.Since(s.started).Seconds())}, nil
	case "profile":
		return s.profileTool(ctx, req.Params)
	case "shutdown":
		s.quitOnce.Do(func() { close(s.quit) })
		return map[string]bool{"ok": true}, nil
	}

	b, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	if req.Method == "run" {
		return s.run(ctx, b, req.Params)
	}
	if pageTools[req.Method] {
		return s.pageTool(ctx, b, req.Method, req.Params)
	}
	switch req.Method {
	case "status":
		v, err := b.Version(ctx)
		if err != nil {
			return nil, err
		}
		pages, err := b.Pages(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"product": v.Product, "protocol": v.ProtocolVersion, "endpoint": b.Endpoint,
			"profile": s.prof.Name, "profileDir": s.prof.Dir, "chromeProfile": s.prof.ChromeProfile, "attach": s.prof.Attach,
			"tabs": len(pages), "pid": os.Getpid(),
		}, nil
	case "open":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.URL == "" {
			return nil, errors.New("open: params.url required")
		}
		return b.Open(ctx, p.URL)
	case "close":
		var p struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.TargetID == "" {
			return nil, errors.New("close: params.targetId required")
		}
		return map[string]bool{"ok": true}, b.CloseTab(ctx, target.ID(p.TargetID))
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}

// Call sends one request to the running daemon.
func Call(ctx context.Context, method string, params, out any) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath())
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	req := Request{Method: method}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return fmt.Errorf("brojevd: %w", err)
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Ensure starts brojevd in the background if it is not answering.
func Ensure(ctx context.Context, extraArgs ...string) error {
	if Call(ctx, "ping", nil, nil) == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, append([]string{"daemon"}, extraArgs...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if Call(ctx, "ping", nil, nil) == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("brojevd did not start; see %s", LogPath())
}
