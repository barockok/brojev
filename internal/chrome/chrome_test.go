package chrome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEndpointFromURL(t *testing.T) {
	var body string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			w.WriteHeader(status)
			w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	host := strings.TrimPrefix(srv.URL, "http://")

	status, body = 200, `{"Browser":"Chrome/155","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`
	for _, base := range []string{srv.URL, srv.URL + "/"} {
		ep, err := EndpointFromURL(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		if want := "ws://" + host + "/devtools/browser/abc"; ep != want {
			t.Fatalf("EndpointFromURL(%q) = %q, want %q", base, ep, want)
		}
	}

	bad := []struct {
		name, base, body, want string
		status                 int
	}{
		{"non-200", srv.URL, "nope", "status 500", 500},
		{"bad json", srv.URL, "{", "bad /json/version", 200},
		{"missing field", srv.URL, `{"Browser":"x"}`, "no webSocketDebuggerUrl", 200},
		{"scheme", "ws://" + host, "", "http or https", 200},
		{"no host", "http://", "", "http or https", 200},
	}
	for _, c := range bad {
		status, body = c.status, c.body
		_, err := EndpointFromURL(ctx, c.base)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.base) {
			t.Fatalf("%s: err = %v, want it to contain %q and the URL %q", c.name, err, c.want, c.base)
		}
	}
	// redirects are not followed
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/json/version", http.StatusFound)
	}))
	defer red.Close()
	if _, err := EndpointFromURL(ctx, red.URL); err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("redirect must not be followed: %v", err)
	}
	// nothing listening
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if _, err := EndpointFromURL(ctx, deadURL); err == nil || !strings.Contains(err.Error(), deadURL) {
		t.Fatalf("dead endpoint: %v", err)
	}
}

func TestEndpointFromURLHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"webSocketDebuggerUrl":"ws://localhost/devtools/browser/x"}`))
	}))
	defer srv.Close()
	// httptest's self-signed cert: swap in its client transport for this test
	old := httpClient.Transport
	httpClient.Transport = srv.Client().Transport
	defer func() { httpClient.Transport = old }()
	ep, err := EndpointFromURL(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := "wss://" + strings.TrimPrefix(srv.URL, "https://") + "/devtools/browser/x"; ep != want {
		t.Fatalf("got %q, want %q", ep, want)
	}
}

func TestEndpointFromURLRedactsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	live := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	ctx := context.Background()
	_, err := EndpointFromURL(ctx, live)
	srv.Close()
	_, err2 := EndpointFromURL(ctx, live) // nothing listening now
	_, err3 := EndpointFromURL(ctx, "ftp://user:s3cret@host")
	for _, e := range []error{err, err2, err3} {
		if e == nil || strings.Contains(e.Error(), "s3cret") || strings.Contains(e.Error(), "user:") {
			t.Fatalf("error leaks credentials or is nil: %v", e)
		}
	}
	if got := RedactURL("http://a:b@h:1/x"); got != "http://redacted@h:1/x" {
		t.Fatalf("RedactURL = %q", got)
	}
}
