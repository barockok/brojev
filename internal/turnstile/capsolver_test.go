package turnstile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCapSolverSolve(t *testing.T) {
	var creates, polls int
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/createTask":
			creates++
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"errorId":0,"status":"idle","taskId":"task-1"}`))
		case "/getTaskResult":
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"errorId":0,"status":"processing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"errorId":0,"status":"ready","solution":{"token":"tok-123"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &CapSolver{Key: "key-1", Endpoint: srv.URL, HTTP: srv.Client(), Poll: time.Millisecond}
	tok, err := c.Solve(context.Background(), Target{URL: "https://example.com/login", Sitekey: "site-1", Action: "login", CData: "cd-1"})
	if err != nil || tok != "tok-123" {
		t.Fatalf("Solve = %q, %v", tok, err)
	}
	if creates != 1 || polls != 2 {
		t.Fatalf("creates=%d polls=%d", creates, polls)
	}
	task := createBody["task"].(map[string]any)
	if task["type"] != "AntiTurnstileTaskProxyLess" || task["websiteURL"] != "https://example.com/login" || task["websiteKey"] != "site-1" {
		t.Fatalf("task = %#v", task)
	}
	md, _ := task["metadata"].(map[string]any)
	if md["action"] != "login" || md["cdata"] != "cd-1" {
		t.Fatalf("metadata = %#v", md)
	}
}

func TestCapSolverAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorId":1,"errorCode":"ERROR_KEY_DENIED_ACCESS","errorDescription":"denied"}`))
	}))
	defer srv.Close()
	c := &CapSolver{Key: "bad", Endpoint: srv.URL, HTTP: srv.Client(), Poll: time.Millisecond}
	_, err := c.Solve(context.Background(), Target{URL: "https://x.test", Sitekey: "s"})
	if err == nil || !strings.Contains(err.Error(), "ERROR_KEY_DENIED_ACCESS") {
		t.Fatalf("err = %v", err)
	}
}

func TestCapSolverTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "createTask") {
			_, _ = w.Write([]byte(`{"errorId":0,"taskId":"task-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"errorId":0,"status":"processing"}`))
	}))
	defer srv.Close()
	c := &CapSolver{Key: "k", Endpoint: srv.URL, HTTP: srv.Client(), Poll: 2 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Solve(ctx, Target{URL: "https://x.test", Sitekey: "s"})
	if err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestCapSolverRequiresSitekey(t *testing.T) {
	c := NewCapSolver("k")
	_, err := c.Solve(context.Background(), Target{URL: "https://x.test"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestStatusLine(t *testing.T) {
	cases := []struct {
		s    *Status
		want string
	}{
		{nil, ""},
		{&Status{State: StateAbsent}, ""},
		{&Status{Detected: true, State: StateSolved}, "TURNSTILE: solved"},
		{&Status{Detected: true, State: StateSolved, Widgets: 2}, "TURNSTILE: solved"},
		{&Status{Detected: true, State: StateUnresolved, Reason: "solver disabled"}, "TURNSTILE: unresolved (solver disabled)"},
		{&Status{Detected: true, State: StateUnresolved, Widgets: 2, Unsolved: 1}, "TURNSTILE: unresolved (1 of 2 widgets)"},
		{&Status{Detected: true, State: StateUnresolved, Widgets: 2, Unsolved: 1, Reason: "1 of 2 widgets unsolved: waiting for token"}, "TURNSTILE: unresolved (1 of 2 widgets unsolved: waiting for token)"},
		{&Status{Detected: true, State: StateInterstitial}, "TURNSTILE: interstitial (full-page Cloudflare challenge)"},
	}
	for _, c := range cases {
		if got := c.s.Line(); got != c.want {
			t.Errorf("Line() = %q, want %q", got, c.want)
		}
	}
}
