package cdp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/chromedp/cdproto/target"
	"github.com/coder/websocket"
)

// fakeBrowser answers commands with handle and lets the test push events.
func fakeBrowser(t *testing.T, handle func(m *message) *message) (url string, events chan *message) {
	t.Helper()
	events = make(chan *message, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		go func() {
			for ev := range events {
				b, _ := jsonv2.Marshal(ev)
				_ = ws.Write(ctx, websocket.MessageText, b)
			}
		}()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			m := new(message)
			if err := jsonv2.Unmarshal(data, m); err != nil {
				t.Errorf("bad frame: %v", err)
				return
			}
			resp := handle(m)
			if resp == nil {
				continue // never answer: exercises timeouts
			}
			resp.ID, resp.SessionID = m.ID, m.SessionID
			b, _ := jsonv2.Marshal(resp)
			_ = ws.Write(ctx, websocket.MessageText, b)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), events
}

func dial(t *testing.T, url string) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestTypedCallRoundTrip(t *testing.T) {
	url, _ := fakeBrowser(t, func(m *message) *message {
		if m.Method != "Target.createTarget" || !strings.Contains(string(m.Params), `"url":"https://example.com"`) {
			return &message{Error: &Error{Code: -32601, Message: "unexpected " + m.Method + " " + string(m.Params)}}
		}
		return &message{Result: jsonv2.Value(`{"targetId":"T1"}`)}
	})
	c := dial(t, url)
	res, err := cdproto.Call(context.Background(), c.Browser(), target.CreateTarget, target.CreateTargetParams{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TargetID != "T1" {
		t.Fatalf("targetId = %q", res.TargetID)
	}
}

func TestBrowserErrorIsTyped(t *testing.T) {
	url, _ := fakeBrowser(t, func(m *message) *message {
		return &message{Error: &Error{Code: -32000, Message: "No target with given id"}}
	})
	c := dial(t, url)
	err := c.Browser().Call(context.Background(), "Target.closeTarget", map[string]string{"targetId": "x"}, nil)
	var cerr *Error
	if !errors.As(err, &cerr) || cerr.Code != -32000 || cerr.Method != "Target.closeTarget" {
		t.Fatalf("err = %v", err)
	}
}

func TestCallTimesOutAndForgets(t *testing.T) {
	url, _ := fakeBrowser(t, func(m *message) *message { return nil })
	c := dial(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Browser().Call(ctx, "Runtime.evaluate", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending leaked: %d", n)
	}
}

func TestEventsRouteBySessionAndBuffer(t *testing.T) {
	url, events := fakeBrowser(t, func(m *message) *message { return &message{} })
	c := dial(t, url)
	ch, cancel := c.Session("S1").Subscribe("Page.loadEventFired")
	defer cancel()
	other, cancelOther := c.Session("S2").Subscribe("Page.loadEventFired")
	defer cancelOther()

	// Burst more events than any fixed buffer before reading any.
	for i := 0; i < 10; i++ {
		events <- &message{SessionID: "S1", Method: "Page.loadEventFired", Params: jsonv2.Value(`{"timestamp":1}`)}
	}
	// Round trip guarantees the reader has processed the burst.
	if err := c.Browser().Call(context.Background(), "Browser.getVersion", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("event %d missing", i)
		}
	}
	select {
	case v := <-other:
		t.Fatalf("S2 got S1 event: %s", v)
	default:
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	url, _ := fakeBrowser(t, func(m *message) *message { return nil })
	c, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- c.Browser().Call(context.Background(), "Runtime.evaluate", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call not released on close")
	}
}
