package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barockok/brojev/internal/profile"
)

func TestResolveSession(t *testing.T) {
	cases := []struct {
		req, params, id, base string
		named                 bool
		profile               string
	}{
		{"", ``, "default", "default", false, ""},
		{"", `{"session":""}`, "default", "default", false, ""},
		{"abc", ``, "abc", "abc", false, ""},
		{"abc", `{"url":"x"}`, "abc", "abc", false, ""},
		{"abc", `{"session":"r1"}`, "abc#r1", "abc", true, ""},
		{"", `{"session":"r1"}`, "default#r1", "default", true, ""},
		{"abc", `{"profile":"work"}`, "abc", "abc", false, "work"},
		{"abc", `{"session":"r1","profile":"work"}`, "abc#r1", "abc", true, "work"},
	}
	for _, c := range cases {
		got := resolveSession(c.req, json.RawMessage(c.params))
		want := sessionReq{id: c.id, base: c.base, named: c.named, profile: c.profile}
		if got != want {
			t.Errorf("resolveSession(%q, %s) = %+v; want %+v", c.req, c.params, got, want)
		}
	}
}

// profileHome is a brojev home with the profiles default, work and play.
func profileHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_PROFILE", "")
	t.Setenv("BROJEV_ATTACH", "")
	t.Setenv("BROJEV_CDP_URL", "")
	cfg, err := profile.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"work", "play"} {
		if _, err := cfg.Add(home, profile.Profile{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	return home
}

func named(base, name, prof string) sessionReq {
	return sessionReq{id: base + subSep + name, base: base, named: true, profile: prof}
}

func TestSessionIsLockedToItsProfile(t *testing.T) {
	profileHome(t)
	s := &Server{}

	a, err := s.sessionFor(named("x", "a", "work"))
	if err != nil || a.prof.Name != "work" {
		t.Fatalf("new session on work: %+v %v", a, err)
	}
	// The same name comes back to the same window, with or without the profile.
	for _, prof := range []string{"", "work"} {
		if again, err := s.sessionFor(named("x", "a", prof)); err != nil || again != a {
			t.Fatalf("same session (profile=%q): %p %v, want %p", prof, again, err, a)
		}
	}
	// It cannot move to another profile.
	_, err = s.sessionFor(named("x", "a", "play"))
	if err == nil || !strings.Contains(err.Error(), `"work"`) || !strings.Contains(err.Error(), `"play"`) {
		t.Fatalf("moving a session: err = %v", err)
	}
	// A new name is a new window: on the same profile it shares that Chrome...
	b, err := s.sessionFor(named("x", "b", "work"))
	if err != nil || b == a || b.ln != a.ln {
		t.Fatalf("second session on work: %+v %v", b, err)
	}
	// ...and on another profile it gets another Chrome.
	c, err := s.sessionFor(named("x", "c", "play"))
	if err != nil || c.prof.Name != "play" || c.ln == a.ln {
		t.Fatalf("session on play: %+v %v", c, err)
	}
	// No profile: the active one.
	d, err := s.sessionFor(named("x", "d", ""))
	if err != nil || d.prof.Name != "default" {
		t.Fatalf("session without a profile: %+v %v", d, err)
	}
	if _, err := s.sessionFor(named("x", "e", "nope")); err == nil || !strings.Contains(err.Error(), "work") {
		t.Fatalf("unknown profile: err = %v", err)
	}
}

// A caller that names no session has one window per profile.
func TestUnnamedSessionPerProfile(t *testing.T) {
	home := profileHome(t)
	s := &Server{}
	req := func(prof string) sessionReq { return sessionReq{id: "x", base: "x", profile: prof} }

	def, err := s.sessionFor(req(""))
	if err != nil || def.prof.Name != "default" {
		t.Fatalf("unnamed default: %+v %v", def, err)
	}
	work, err := s.sessionFor(req("work"))
	if err != nil || work == def || work.prof.Name != "work" || work.ln == def.ln {
		t.Fatalf("unnamed on work: %+v %v", work, err)
	}
	if again, _ := s.sessionFor(req("work")); again != work {
		t.Fatal("unnamed on work must come back to the same window")
	}
	if again, _ := s.sessionFor(req("")); again != def {
		t.Fatal("unnamed without a profile must stay on the default one")
	}
	// After "profile use", no profile means the new default; the old window stays.
	cfg, _ := profile.Load(home)
	cfg.Active = "work"
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.sessionFor(req("")); got != work {
		t.Fatal("unnamed without a profile must follow the default profile")
	}
	if work.key == def.key || stateFile(work) == stateFile(def) {
		t.Fatalf("sessions of two profiles share a key or state file: %q %q", work.key, stateFile(work))
	}
}

// Two profiles on one Chrome data dir cannot run side by side.
func TestSharedDataDir(t *testing.T) {
	home := profileHome(t)
	cfg, _ := profile.Load(home)
	dir := filepath.Join(home, "shared")
	for n, sub := range map[string]string{"p1": "Default", "p2": "Profile 1", "p1b": "Default"} {
		if _, err := cfg.Add(home, profile.Profile{Name: n, Dir: dir, ChromeProfile: sub}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	a, err := s.sessionFor(named("x", "a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	// Same dir and same person under another name: the same Chrome.
	if b, err := s.sessionFor(named("x", "b", "p1b")); err != nil || b.ln != a.ln {
		t.Fatalf("alias of the same Chrome profile: %+v %v", b, err)
	}
	// Another person in the same dir while sessions work there: refused.
	_, err = s.sessionFor(named("x", "c", "p2"))
	if err == nil || !strings.Contains(err.Error(), "one Chrome profile at a time") {
		t.Fatalf("second person in a busy data dir: err = %v", err)
	}
	// Once they are gone the dir can switch.
	s.reap("x")
	if c, err := s.sessionFor(named("x", "c", "p2")); err != nil || c.ln.prof.Name != "p2" {
		t.Fatalf("second person in an idle data dir: %+v %v", c, err)
	}
}

func TestExternalChromeHasNoProfiles(t *testing.T) {
	profileHome(t)
	t.Setenv("BROJEV_CDP_URL", "http://127.0.0.1:1")
	s := &Server{}
	if sess, err := s.sessionFor(named("x", "a", "")); err != nil || sess.prof.Name != "external" {
		t.Fatalf("session on the external Chrome: %+v %v", sess, err)
	}
	if _, err := s.sessionFor(named("x", "b", "work")); err == nil || !strings.Contains(err.Error(), "BROJEV_CDP_URL") {
		t.Fatalf("a profile on the external Chrome: err = %v", err)
	}
}

func TestVersionOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.4.1", true},
		{"0.4.1", "0.4.1", false},
		{"0.5.0", "0.4.9", false},
		{"0.10.0", "0.9.0", false},
		{"v0.4.1", "0.4.2", true},
		{"0.4.1-dev", "0.4.1", false},
		{"weird", "0.4.1", true},
		{"", "0.4.1", true},
	}
	for _, c := range cases {
		if got := versionOlder(c.a, c.b); got != c.want {
			t.Errorf("versionOlder(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSessionLimit(t *testing.T) {
	profileHome(t)
	s := &Server{}
	for i := 0; i < maxSessions; i++ {
		if _, err := s.sessionFor(named("x", string(rune('a'+i)), "")); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, err := s.sessionFor(named("x", "one-too-many", "")); err == nil {
		t.Fatal("expected a session limit error")
	}
	if _, err := s.sessionFor(named("x", "a", "")); err != nil {
		t.Fatalf("existing session must stay reachable: %v", err)
	}
	if _, err := s.sessionFor(sessionReq{id: defaultSession, base: defaultSession}); err != nil {
		t.Fatalf("default session is exempt from the limit: %v", err)
	}
}
