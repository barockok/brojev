package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/browser"
)

// Several Claude sessions (and subagents) share one brojevd and one Chrome.
// Each gets a session: its own current tab, its own tabs (each in a window of
// its own, so no two sessions fight over the foreground tab) and its own
// action queue. Requests without a session id belong to the "default"
// session, which keeps the pre-session behaviour for the CLI.

const (
	defaultSession = "default"
	maxSessions    = 8
	subSep         = "#"
)

type session struct {
	id    string
	base  string // the lease holder: the id without the "#subagent" suffix
	sem   chan struct{}
	cur   target.ID // guarded by Server.mu
	owned map[target.ID]bool
}

func (s *session) isDefault() bool { return s.id == defaultSession }

// resolveSession turns the request's session id and the optional per-call
// "session" param (a subagent name) into the id that owns the call.
func resolveSession(reqSession string, params json.RawMessage) (id, base string) {
	base = reqSession
	if base == "" {
		base = defaultSession
	}
	var p struct {
		Session string `json:"session"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.Session == "" {
		return base, base
	}
	return base + subSep + p.Session, base
}

// sessionFor returns the session, creating it (up to maxSessions).
func (s *Server) sessionFor(id, base string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	if sess := s.sessions[id]; sess != nil {
		return sess, nil
	}
	if id != defaultSession && s.namedLocked() >= maxSessions {
		return nil, fmt.Errorf("too many concurrent brojev sessions (max %d): close one or wait for an idle session to expire", maxSessions)
	}
	sess := &session{id: id, base: base, sem: make(chan struct{}, 1), owned: map[target.ID]bool{}}
	s.sessions[id] = sess
	return sess, nil
}

// acquire serialises the calls of one session; sessions run in parallel.
func (sess *session) acquire(ctx context.Context) (release func(), err error) {
	select {
	case sess.sem <- struct{}{}:
		return func() { <-sess.sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the previous action of this session: %w", ctx.Err())
	}
}

// claim records that sess created (or was handed) the tab.
func (s *Server) claim(sess *session, id target.ID) {
	if sess.isDefault() {
		return
	}
	s.mu.Lock()
	if s.owner == nil {
		s.owner = map[target.ID]string{}
	}
	s.owner[id] = sess.id
	sess.owned[id] = true
	s.mu.Unlock()
}

// unclaim forgets a closed tab.
func (s *Server) unclaim(id target.ID) {
	s.mu.Lock()
	if o := s.owner[id]; o != "" {
		if sess := s.sessions[o]; sess != nil {
			delete(sess.owned, id)
		}
	}
	delete(s.owner, id)
	s.mu.Unlock()
}

// canTouch reports whether sess may switch to or close the tab. Tabs of
// another named session need an explicit shared flag.
func (s *Server) canTouch(sess *session, id target.ID, shared bool) bool {
	s.mu.Lock()
	o := s.owner[id]
	s.mu.Unlock()
	if sess.isDefault() {
		return o == "" || shared
	}
	return o == sess.id || shared
}

// sessionPages lists the tabs sess works with: the ones it owns (plus popups
// they opened), or for the default session every tab no named session owns.
func (s *Server) sessionPages(ctx context.Context, b *browser.Browser, sess *session) ([]*target.Info, error) {
	pages, err := b.Pages(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == nil {
		s.owner = map[target.ID]string{}
	}
	alive := make(map[target.ID]bool, len(pages))
	for _, t := range pages {
		alive[t.TargetID] = true
	}
	for id, o := range s.owner { // tabs closed behind our back
		if !alive[id] {
			if ow := s.sessions[o]; ow != nil {
				delete(ow.owned, id)
			}
			delete(s.owner, id)
		}
	}
	// A popup belongs to the session of the tab that opened it.
	for changed := true; changed; {
		changed = false
		for _, t := range pages {
			if s.owner[t.TargetID] != "" || t.OpenerID == "" {
				continue
			}
			if o := s.owner[t.OpenerID]; o != "" {
				if ow := s.sessions[o]; ow != nil {
					s.owner[t.TargetID] = o
					ow.owned[t.TargetID] = true
					changed = true
				}
			}
		}
	}
	want := sess.id
	if sess.isDefault() {
		want = ""
	}
	var out []*target.Info
	for _, t := range pages {
		if s.owner[t.TargetID] == want {
			out = append(out, t)
		}
	}
	return out, nil
}

// ---- leases: a brojev-mcp process holds one connection open; when it goes
// away its sessions are cleaned up after a grace period.

func graceDuration() time.Duration {
	if v := os.Getenv("BROJEV_SESSION_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 60 * time.Second
}

func (s *Server) leaseAdd(base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		s.leases = map[string]int{}
	}
	s.leases[base]++
	if t := s.pending[base]; t != nil { // reconnect within the grace period
		t.Stop()
		delete(s.pending, base)
	}
}

func (s *Server) leaseDrop(base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[base]--; s.leases[base] > 0 {
		return
	}
	delete(s.leases, base)
	if s.pending == nil {
		s.pending = map[string]*time.Timer{}
	}
	s.pending[base] = time.AfterFunc(graceDuration(), func() { s.reap(base) })
}

// reap drops the sessions of base. Their tabs are closed only on the
// dedicated automation profile; in attach mode (the user's real Chrome) the
// windows stay open for the user to keep or close.
func (s *Server) reap(base string) {
	s.mu.Lock()
	delete(s.pending, base)
	if s.leases[base] > 0 {
		s.mu.Unlock()
		return
	}
	var tabs []target.ID
	for id, sess := range s.sessions {
		if sess.base != base || sess.isDefault() {
			continue
		}
		for t := range sess.owned {
			tabs = append(tabs, t)
			delete(s.owner, t)
			if p := s.pages[t]; p != nil {
				p.Close()
				delete(s.pages, t)
			}
		}
		delete(s.sessions, id)
	}
	b, attach := s.browser, s.prof.Attach || CDPURL() != ""
	s.mu.Unlock()
	if b == nil || attach {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, t := range tabs {
		_ = b.CloseTab(ctx, t)
	}
	if len(tabs) > 0 {
		log.Printf("session %s ended: closed %d tab(s)", base, len(tabs))
	}
}

// otherLiveBases lists leased sessions other than the caller's.
func (s *Server) otherLiveBases(caller string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for base, n := range s.leases {
		if n > 0 && base != caller {
			out = append(out, base)
		}
	}
	return out
}

// stateFile is the default state_save/state_load path of a session.
func stateFile(sess *session) string {
	name := "state.json"
	if !sess.isDefault() {
		clean := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, sess.id)
		name = "state-" + clean + ".json"
	}
	return name
}

var errNotYours = errors.New("that tab belongs to another brojev session; pass shared=true to use it anyway")

func (s *Server) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namedLocked()
}

// namedLocked counts the sessions other than the default one.
func (s *Server) namedLocked() int {
	n := 0
	for id := range s.sessions {
		if id != defaultSession {
			n++
		}
	}
	return n
}

// versionOlder reports whether dotted version a is older than b. Anything
// unparsable counts as older, so a daemon of unknown version gets replaced.
func versionOlder(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return a != b
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) (out [3]int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return out, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// LeaseConn is the open connection that tells brojevd a brojev-mcp process
// is alive. When it closes (clean exit or crash) the daemon cleans up the
// process's sessions after a grace period.
type LeaseConn struct {
	c    net.Conn
	done chan struct{}
}

// Done is closed when the daemon end goes away (restart, upgrade).
func (l *LeaseConn) Done() <-chan struct{} { return l.done }
func (l *LeaseConn) Close() error          { return l.c.Close() }

// Lease registers session with the running daemon and holds the connection.
func Lease(ctx context.Context, session string) (*LeaseConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath())
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if err := json.NewEncoder(c).Encode(Request{Method: "hello", Session: session}); err != nil {
		c.Close()
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		c.Close()
		return nil, fmt.Errorf("brojevd: %w", err)
	}
	if resp.Error != "" {
		c.Close()
		return nil, errors.New(resp.Error)
	}
	c.SetDeadline(time.Time{})
	l := &LeaseConn{c: c, done: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(l.done)
	}()
	return l, nil
}
