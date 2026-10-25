package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
