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
	"github.com/barockok/brojev/internal/profile"
)

// Several Claude sessions (and subagents) share one brojevd. Each gets a
// session: a window of its own in the Chrome of one profile, with its own
// current tab, its own tabs and its own action queue. A session never changes
// profile; a different profile means a different session, so one agent can
// drive several profiles side by side. Requests without a session id belong
// to the "default" session, which keeps the pre-session behaviour for the CLI.

const (
	defaultSession = "default"
	maxSessions    = 8
	subSep         = "#"
	profSep        = "@" // unnamed sessions: one per profile
)

type session struct {
	id    string
	key   string // its key in Server.sessions and the owner maps
	base  string // the lease holder: the id without the "#subagent" suffix
	prof  profile.Profile
	ln    *link // the Chrome of prof; never changes
	sem   chan struct{}
	cur   target.ID // guarded by Server.mu
	owned map[target.ID]bool
}

func (s *session) isDefault() bool { return s.id == defaultSession }

// sessionReq is what one call says about the session it belongs to.
type sessionReq struct {
	id, base string
	named    bool   // the caller picked a session name
	profile  string // the profile the call asked for; empty means no preference
}

// resolveSession turns the request's session id and the optional per-call
// "session" (a name the agent picked) and "profile" params into the session
// that owns the call.
func resolveSession(reqSession string, params json.RawMessage) sessionReq {
	r := sessionReq{base: reqSession}
	if r.base == "" {
		r.base = defaultSession
	}
	var p struct {
		Session string `json:"session"`
		Profile string `json:"profile"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	r.id, r.profile = r.base, p.Profile
	if p.Session != "" {
		r.id, r.named = r.base+subSep+p.Session, true
	}
	return r
}

// sessionFor returns the session, creating it (up to maxSessions).
//
// A named session is locked to the profile it was created on: asking for it
// with another profile is an error. A caller that names no session gets one
// window per profile, so "no name" always means "my window on that profile".
func (s *Server) sessionFor(r sessionReq) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*session{}
	}
	if r.named {
		if sess := s.sessions[r.id]; sess != nil {
			if r.profile != "" && r.profile != sess.prof.Name {
				return nil, fmt.Errorf("session %q is on profile %q and cannot move to %q: use another session name for that profile",
					strings.TrimPrefix(r.id, r.base+subSep), sess.prof.Name, r.profile)
			}
			return sess, nil
		}
	}
	prof, err := resolveProfile(r.profile)
	if err != nil {
		return nil, err
	}
	key := r.id
	if !r.named {
		key += profSep + prof.Name
		if sess := s.sessions[key]; sess != nil {
			return sess, nil
		}
	}
	if r.id != defaultSession && s.namedLocked() >= maxSessions {
		return nil, fmt.Errorf("too many concurrent brojev sessions (max %d): close one or wait for an idle session to expire", maxSessions)
	}
	ln, err := s.linkForLocked(prof)
	if err != nil {
		return nil, err
	}
	sess := &session{id: r.id, key: key, base: r.base, prof: prof, ln: ln, sem: make(chan struct{}, 1), owned: map[target.ID]bool{}}
	s.sessions[key] = sess
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
	sess.ln.owner[id] = sess.key
	sess.owned[id] = true
	s.mu.Unlock()
}

// unclaim forgets a closed tab of ln.
func (s *Server) unclaim(ln *link, id target.ID) {
	s.mu.Lock()
	if o := ln.owner[id]; o != "" {
		if sess := s.sessions[o]; sess != nil {
			delete(sess.owned, id)
		}
	}
	delete(ln.owner, id)
	s.mu.Unlock()
}

// canTouch reports whether sess may switch to or close the tab. Tabs of
// another named session need an explicit shared flag.
func (s *Server) canTouch(sess *session, id target.ID, shared bool) bool {
	s.mu.Lock()
	o := sess.ln.owner[id]
	s.mu.Unlock()
	if sess.isDefault() {
		return o == "" || shared
	}
	return o == sess.key || shared
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
	owner := sess.ln.owner
	alive := make(map[target.ID]bool, len(pages))
	for _, t := range pages {
		alive[t.TargetID] = true
	}
	for id, o := range owner { // tabs closed behind our back
		if !alive[id] {
			if ow := s.sessions[o]; ow != nil {
				delete(ow.owned, id)
			}
			delete(owner, id)
		}
	}
	// A popup belongs to the session of the tab that opened it.
	for changed := true; changed; {
		changed = false
		for _, t := range pages {
			if owner[t.TargetID] != "" || t.OpenerID == "" {
				continue
			}
			if o := owner[t.OpenerID]; o != "" {
				if ow := s.sessions[o]; ow != nil {
					owner[t.TargetID] = o
					ow.owned[t.TargetID] = true
					changed = true
				}
			}
		}
	}
	want := sess.key
	if sess.isDefault() {
		want = ""
	}
	var out []*target.Info
	for _, t := range pages {
		if owner[t.TargetID] == want {
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
	type closing struct {
		b  *browser.Browser
		id target.ID
	}
	var tabs []closing
	for key, sess := range s.sessions {
		if sess.base != base || sess.isDefault() {
			continue
		}
		ln := sess.ln
		keep := ln.b == nil || ln.keepLocked()
		for t := range sess.owned {
			if !keep {
				tabs = append(tabs, closing{ln.b, t})
			}
			delete(ln.owner, t)
			if p := ln.pages[t]; p != nil {
				p.Close()
				delete(ln.pages, t)
			}
		}
		delete(s.sessions, key)
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, t := range tabs {
		_ = t.b.CloseTab(ctx, t.id)
	}
	if len(tabs) > 0 {
		log.Printf("session %s ended: closed %d tab(s)", base, len(tabs))
	}
}

// stateFile is the default state_save/state_load path of a session. Logins
// of one profile never land in the default file of another.
func stateFile(sess *session) string {
	clean := func(v string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, v)
	}
	name := "state"
	if !sess.isDefault() {
		name += "-" + clean(sess.id)
	}
	if n := sess.prof.Name; n != "" && n != "default" {
		name += "." + clean(n)
	}
	return name + ".json"
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
	for _, sess := range s.sessions {
		if !sess.isDefault() {
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
