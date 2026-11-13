package daemon

import (
	"encoding/json"
	"testing"
)

func TestResolveSession(t *testing.T) {
	cases := []struct {
		req, params, id, base string
	}{
		{"", ``, "default", "default"},
		{"", `{"session":""}`, "default", "default"},
		{"abc", ``, "abc", "abc"},
		{"abc", `{"url":"x"}`, "abc", "abc"},
		{"abc", `{"session":"r1"}`, "abc#r1", "abc"},
		{"", `{"session":"r1"}`, "default#r1", "default"},
	}
	for _, c := range cases {
		id, base := resolveSession(c.req, json.RawMessage(c.params))
		if id != c.id || base != c.base {
			t.Errorf("resolveSession(%q, %s) = %q, %q; want %q, %q", c.req, c.params, id, base, c.id, c.base)
		}
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
	s := &Server{}
	for i := 0; i < maxSessions; i++ {
		if _, err := s.sessionFor(string(rune('a'+i)), "x"); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, err := s.sessionFor("one-too-many", "x"); err == nil {
		t.Fatal("expected a session limit error")
	}
	if _, err := s.sessionFor("a", "x"); err != nil {
		t.Fatalf("existing session must stay reachable: %v", err)
	}
	if _, err := s.sessionFor(defaultSession, defaultSession); err != nil {
		t.Fatalf("default session is exempt from the limit: %v", err)
	}
}
