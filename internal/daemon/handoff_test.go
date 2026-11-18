package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeBrowser accepts a Storage.setCookies batch only if no cookie carries a
// field outside allow, like an engine with a narrower cookie model.
type fakeBrowser struct {
	allow map[string]bool
	set   []map[string]any
	calls int
}

func (f *fakeBrowser) Call(_ context.Context, method string, params, _ any) error {
	f.calls++
	if method != "Storage.setCookies" {
		return errors.New("unexpected " + method)
	}
	cs := params.(map[string]any)["cookies"].([]map[string]any)
	for _, c := range cs {
		if c["name"] == "bad" {
			return errors.New("rejected")
		}
		for k := range c {
			if !f.allow[k] {
				return errors.New("unknown field " + k)
			}
		}
	}
	f.set = append(f.set, cs...)
	return nil
}

func TestSetCookiesFallback(t *testing.T) {
	cookies := []map[string]any{
		{"name": "sid", "value": "1", "domain": "a.test", "path": "/", "httpOnly": true, "sameSite": "Lax", "session": true, "expires": -1.0, "size": 4},
		{"name": "bad", "value": "x", "domain": "a.test", "path": "/"},
	}
	all := map[string]bool{"name": true, "value": true, "domain": true, "path": true, "secure": true, "httpOnly": true, "sameSite": true, "expires": true}
	// Whole batch accepted only without the rejected cookie: per-cookie retry.
	f := &fakeBrowser{allow: all}
	if ok, failed := setCookies(context.Background(), f, cookies); ok != 1 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 1, 1", ok, failed)
	}
	if _, has := f.set[0]["expires"]; has {
		t.Fatalf("session cookie kept expires: %v", f.set[0])
	}
	if _, has := f.set[0]["size"]; has {
		t.Fatalf("read-only field copied: %v", f.set[0])
	}
	// An engine without sameSite gets the minimal field set.
	narrow := map[string]bool{"name": true, "value": true, "domain": true, "path": true, "secure": true, "httpOnly": true, "expires": true}
	f = &fakeBrowser{allow: narrow}
	if ok, failed := setCookies(context.Background(), f, cookies[:1]); ok != 1 || failed != 0 {
		t.Fatalf("narrow: ok=%d failed=%d, want 1, 0", ok, failed)
	}
	if f.set[0]["httpOnly"] != true {
		t.Fatalf("httpOnly lost: %v", f.set[0])
	}
	// Nothing to copy: no calls.
	f = &fakeBrowser{allow: all}
	if ok, failed := setCookies(context.Background(), f, nil); ok != 0 || failed != 0 || f.calls != 0 {
		t.Fatalf("empty: ok=%d failed=%d calls=%d", ok, failed, f.calls)
	}
}

func TestHandoffOffByDefault(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_HUMAN_CDP_URL", "")
	t.Setenv("BROJEV_CHROME", "/nonexistent/chrome") // must not try to launch anything
	s := &Server{}
	for _, action := range []string{"start", "finish", "status", ""} {
		raw, _ := json.Marshal(map[string]string{"action": action})
		_, err := s.dispatch(context.Background(), Request{Method: "handoff", Params: raw})
		if err == nil || err.Error() != errNoHuman.Error() {
			t.Fatalf("%q: err = %v, want %v", action, err, errNoHuman)
		}
	}
}

func TestHandoffParams(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_HUMAN_CDP_URL", "http://127.0.0.1:1") // never reached
	s := &Server{}
	if _, err := s.handoffTool(context.Background(), json.RawMessage(`{"action":"bogus"}`)); err == nil || !strings.Contains(err.Error(), "start, finish or status") {
		t.Fatalf("bogus action: %v", err)
	}
	if _, err := s.handoffTool(context.Background(), json.RawMessage(`{"action":"finish"}`)); err == nil || !strings.Contains(err.Error(), "none open") {
		t.Fatalf("finish without start: %v", err)
	}
	if got := originOf("https://a.test:8443/x?y#z"); got != "https://a.test:8443" {
		t.Fatalf("originOf = %q", got)
	}
	if got := originOf("about:blank"); got != "" {
		t.Fatalf("originOf(about:blank) = %q", got)
	}
}

// A start that fails (here: the agent's browser is unreachable) leaves no
// handoff open, so the next start is not refused.
func TestHandoffStartFailureClears(t *testing.T) {
	t.Setenv("BROJEV_HOME", t.TempDir())
	t.Setenv("BROJEV_CDP_URL", "http://127.0.0.1:1")
	t.Setenv("BROJEV_HUMAN_CDP_URL", "http://127.0.0.1:1")
	s := &Server{}
	for i := 0; i < 2; i++ {
		_, err := s.handoffTool(context.Background(), json.RawMessage(`{"action":"start","url":"https://a.test/"}`))
		if err == nil || strings.Contains(err.Error(), "already open") {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if s.handoff != nil {
		t.Fatalf("handoff left open: %+v", s.handoff)
	}
}
