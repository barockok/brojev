// Package daemon runs brojevd: it owns one CDP connection per Chrome profile
// in use (so Chrome asks for remote-debugging approval at most once) and
// serves short-lived clients (MCP server, CLI) over a unix socket with
// newline-delimited JSON.
package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/barockok/brojev/internal/version"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	Method  string          `json:"method"`
	Session string          `json:"session,omitempty"` // the calling brojev-mcp process; empty is the default session
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Server struct {
	Headless bool

	mu       sync.Mutex
	links    map[string]*link // data dir -> the Chrome of the profiles on it
	sessions map[string]*session
	leases   map[string]int // session base -> open lease connections
	pending  map[string]*time.Timer
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
	var leased []string
	defer func() {
		for _, base := range leased {
			s.leaseDrop(base)
		}
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req Request
		var resp Response
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp.Error = "bad request: " + err.Error()
		} else if req.Method == "hello" {
			// A brojev-mcp process keeps this connection open for as long as it lives.
			if req.Session == "" {
				resp.Error = "hello: session required"
			} else {
				s.leaseAdd(req.Session)
				leased = append(leased, req.Session)
				resp.Result = json.RawMessage(`{"ok":true}`)
			}
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

// CDPURL is BROJEV_CDP_URL: the DevTools HTTP base of a Chrome someone else
// runs. When set, brojev attaches to it and never launches, quits or
// relaunches it.
func CDPURL() string { return os.Getenv("BROJEV_CDP_URL") }

var errExternalChrome = errors.New("BROJEV_CDP_URL is set: brojev does not manage this Chrome")

// link is the connection to the Chrome of one profile data dir. Chrome runs
// one instance per data dir, so profiles that share a dir share a link.
type link struct {
	key    string
	connMu sync.Mutex // serialises (re)connecting, which can take minutes in attach mode

	// guarded by Server.mu
	prof  profile.Profile // profile of the connection
	b     *browser.Browser
	pages map[target.ID]*page.Page
	owner map[target.ID]string // tab -> key of the owning named session
}

// keepLocked reports whether the Chrome is not ours to tidy: the user's real
// Chrome (attach mode) or an external one. Sessions never close tabs there
// on exit, and only close tabs they opened.
func (ln *link) keepLocked() bool { return CDPURL() != "" || ln.prof.Attach }

func (s *Server) keepTabs(ln *link) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ln.keepLocked()
}

// resolveProfile returns the profile a call asked for by name (empty: the
// active one). An external Chrome is the only browser there is.
func resolveProfile(name string) (profile.Profile, error) {
	if base := CDPURL(); base != "" {
		if name != "" && name != "external" {
			return profile.Profile{}, fmt.Errorf("profile %q: %w", name, errExternalChrome)
		}
		return profile.Profile{Name: "external", Dir: chrome.RedactURL(base)}, nil
	}
	return profile.Resolve(Home(), name)
}

func linkKey(p profile.Profile) string {
	if CDPURL() != "" {
		return "external"
	}
	return filepath.Clean(p.Dir)
}

// linkForLocked returns the link of prof's data dir, creating it. A data dir
// that is open as another Chrome profile (person) is switched only while no
// session works there.
func (s *Server) linkForLocked(prof profile.Profile) (*link, error) {
	if s.links == nil {
		s.links = map[string]*link{}
	}
	key := linkKey(prof)
	ln := s.links[key]
	if ln == nil {
		ln = &link{key: key, prof: prof, pages: map[target.ID]*page.Page{}, owner: map[target.ID]string{}}
		s.links[key] = ln
		return ln, nil
	}
	if ln.prof.ChromeProfile == prof.ChromeProfile && ln.prof.Attach == prof.Attach {
		return ln, nil
	}
	if n := s.linkSessionsLocked(ln, ""); n > 0 {
		return nil, fmt.Errorf("profile %q shares the Chrome data dir %s with profile %q, which %d session(s) are using; a data dir runs one Chrome profile at a time",
			prof.Name, prof.Dir, ln.prof.Name, n)
	}
	log.Printf("profile changed %s -> %s; reconnecting", ln.prof.Name, prof.Name)
	s.dropLocked(ln)
	ln.prof = prof
	return ln, nil
}

// linkSessionsLocked counts the named sessions on ln, not counting those of
// the lease holder except.
func (s *Server) linkSessionsLocked(ln *link, except string) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.ln == ln && !sess.isDefault() && (except == "" || sess.base != except) {
			n++
		}
	}
	return n
}

// conn returns the live browser of ln, (re)connecting and launching Chrome if needed.
func (s *Server) conn(ctx context.Context, ln *link) (*browser.Browser, error) {
	ln.connMu.Lock()
	defer ln.connMu.Unlock()
	if base := CDPURL(); base != "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connExternalLocked(ctx, ln, base)
	}
	s.mu.Lock()
	prof := ln.prof
	if ln.b != nil {
		select {
		case <-ln.b.Done():
			s.dropLocked(ln) // Chrome quit or the socket dropped
		default:
			b := ln.b
			s.mu.Unlock()
			return b, nil
		}
	}
	s.mu.Unlock()
	// Launching can wait minutes (attach mode waits for the user's Allow
	// click): do it without s.mu so other sessions' bookkeeping is not blocked.
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
	if err := b.EnableDownloads(ctx, filepath.Join(Home(), "downloads")); err != nil {
		log.Printf("downloads: %v", err)
	}
	s.mu.Lock()
	ln.pages = map[target.ID]*page.Page{}
	ln.b = b
	s.mu.Unlock()
	return b, nil
}

// connExternalLocked attaches to the Chrome at base, resolving its endpoint
// again on every reconnect (a restarted Chrome gets a new browser id), and
// keeps its guard tab open on every call (see browser.EnsureGuard).
func (s *Server) connExternalLocked(ctx context.Context, ln *link, base string) (*browser.Browser, error) {
	if ln.b != nil {
		select {
		case <-ln.b.Done():
			s.dropLocked(ln) // Chrome restarted or the socket dropped
		default:
			s.ensureGuardLocked(ctx, ln)
			return ln.b, nil
		}
	}
	ep, err := chrome.EndpointFromURL(ctx, base)
	if err != nil {
		return nil, err
	}
	b, err := browser.Connect(ctx, ep)
	if err != nil {
		return nil, err
	}
	log.Printf("attached to %s (external Chrome)", chrome.RedactURL(base))
	dl := filepath.Join(Home(), "downloads")
	if err := b.EnableDownloads(ctx, dl); err != nil {
		log.Printf("downloads: %v", err)
	} else {
		log.Printf("downloads: set the external Chrome's download dir to %s", dl)
	}
	ln.pages = map[target.ID]*page.Page{}
	ln.b = b
	s.ensureGuardLocked(ctx, ln)
	return b, nil
}

// ensureGuardLocked keeps the guard tab and makes sure the daemon never
// drives it. A failure is logged, not returned: the call itself may still work.
func (s *Server) ensureGuardLocked(ctx context.Context, ln *link) {
	if err := ln.b.EnsureGuard(ctx); err != nil {
		log.Printf("guard tab: %v", err)
		return
	}
	g := ln.b.Guard()
	if p := ln.pages[g]; p != nil {
		p.Close()
		delete(ln.pages, g)
	}
	for _, sess := range s.sessions {
		if sess.ln == ln && sess.cur == g {
			sess.cur = ""
		}
	}
}

// dropLocked forgets the live connection of ln (Chrome itself keeps running)
// and the tabs of every session on it, which died with it.
func (s *Server) dropLocked(ln *link) {
	for _, p := range ln.pages {
		p.Close()
	}
	ln.pages = map[target.ID]*page.Page{}
	ln.owner = map[target.ID]string{}
	for _, sess := range s.sessions {
		if sess.ln == ln {
			sess.cur, sess.owned = "", map[target.ID]bool{}
		}
	}
	if ln.b != nil {
		ln.b.Close()
		ln.b = nil
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) (any, error) {
	switch req.Method {
	case "ping":
		return map[string]any{"pid": os.Getpid(), "uptimeSec": int(time.Since(s.started).Seconds()), "version": version.Version}, nil
	case "profile":
		return s.profileTool(ctx, req)
	case "keys":
		return setKeys(req.Params)
	case "shutdown":
		s.quitOnce.Do(func() { close(s.quit) })
		return map[string]bool{"ok": true}, nil
	}

	if req.Method == "run" || pageTools[req.Method] {
		sess, err := s.sessionFor(resolveSession(req.Session, req.Params))
		if err != nil {
			return nil, err
		}
		done, err := sess.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
		b, err := s.conn(ctx, sess.ln)
		if err != nil {
			return nil, err
		}
		if req.Method == "run" {
			return s.run(ctx, b, sess, req.Params)
		}
		return s.pageTool(ctx, b, sess, req.Method, req.Params)
	}

	// Browser-level calls (CLI): the Chrome of the asked or the active profile.
	prof, err := resolveProfile(resolveSession(req.Session, req.Params).profile)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ln, err := s.linkForLocked(prof)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b, err := s.conn(ctx, ln)
	if err != nil {
		return nil, err
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
		out := map[string]any{
			"product": v.Product, "protocol": v.ProtocolVersion, "endpoint": b.Endpoint,
			"profile": prof.Name, "profileDir": prof.Dir, "chromeProfile": prof.ChromeProfile, "attach": prof.Attach,
			"tabs": len(pages), "pid": os.Getpid(), "sessions": s.sessionCount(), "browsers": s.liveBrowsers(),
		}
		if g := b.Guard(); g != "" {
			out["guardTab"] = g
		}
		return out, nil
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

// Call sends one request to the running daemon as the default session, or as
// $BROJEV_SESSION when the CLI user set one.
func Call(ctx context.Context, method string, params, out any) error {
	return CallAs(ctx, os.Getenv("BROJEV_SESSION"), method, params, out)
}

// CallAs sends one request on behalf of a session.
func CallAs(ctx context.Context, session, method string, params, out any) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath())
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	req := Request{Method: method, Session: session}
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

// NoSpawn is BROJEV_NO_SPAWN: this client only talks to a daemon someone
// else runs (e.g. on the host, through a mounted socket) and never starts or
// replaces one.
func NoSpawn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BROJEV_NO_SPAWN"))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// Ensure starts brojevd in the background if it is not answering.
func Ensure(ctx context.Context, extraArgs ...string) error {
	var ping struct {
		Version string `json:"version"`
	}
	if NoSpawn() {
		if err := Call(ctx, "ping", nil, &ping); err != nil {
			return fmt.Errorf("brojev daemon not running at %s (BROJEV_NO_SPAWN is set, so it is not started here): %w", SocketPath(), err)
		}
		if ping.Version != version.Version {
			if ping.Version == "" {
				ping.Version = "unknown"
			}
			return fmt.Errorf("brojev daemon at %s is version %s but this client is %s (BROJEV_NO_SPAWN is set, so it is not replaced; restart the daemon with the matching binary)", SocketPath(), ping.Version, version.Version)
		}
		return nil
	}
	if Call(ctx, "ping", nil, &ping) == nil {
		if ping.Version == version.Version || !versionOlder(ping.Version, version.Version) {
			// Same version, or a newer daemon started by another brojev-mcp:
			// keep it, or two plugin versions would restart each other's
			// daemon forever and wipe every session's state.
			return nil
		}
		// An upgraded binary: replace the old daemon (Chrome keeps running).
		_ = Call(ctx, "shutdown", nil, nil)
		for i := 0; i < 40 && Call(ctx, "ping", nil, nil) == nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
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
