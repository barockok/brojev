// Package cdp is brojev's Chrome DevTools Protocol client: one WebSocket,
// flat sessions, typed commands via cdproto's cdp.Session interface.
package cdp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/cdp/jsonv2"
	"github.com/coder/websocket"
)

// DefaultTimeout bounds every command whose context has no deadline. A JS
// dialog freezes the page, so an unbounded call could hang forever.
const DefaultTimeout = 30 * time.Second

var ErrClosed = errors.New("cdp: connection closed")

// Error is an error returned by the browser for a command.
type Error struct {
	Method  string
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty,omitzero"`
}

func (e *Error) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("%s: %s (%d): %s", e.Method, e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("%s: %s (%d)", e.Method, e.Message, e.Code)
}

type message struct {
	ID        int64        `json:"id,omitempty,omitzero"`
	SessionID string       `json:"sessionId,omitempty,omitzero"`
	Method    string       `json:"method,omitempty,omitzero"`
	Params    jsonv2.Value `json:"params,omitempty,omitzero"`
	Result    jsonv2.Value `json:"result,omitempty,omitzero"`
	Error     *Error       `json:"error,omitempty,omitzero"`
}

type subKey struct{ session, method string }

// Conn is one WebSocket to the browser endpoint.
type Conn struct {
	ws     *websocket.Conn
	nextID atomic.Int64

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]chan *message
	subs    map[subKey]map[*subscription]struct{}
	err     error
	done    chan struct{}
}

// Dial connects to a browser WebSocket URL (ws://host:port/devtools/browser/...).
func Dial(ctx context.Context, url string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp: dial %s: %w", url, err)
	}
	ws.SetReadLimit(256 << 20) // snapshots and screenshots can be large
	c := &Conn{
		ws:      ws,
		pending: map[int64]chan *message{},
		subs:    map[subKey]map[*subscription]struct{}{},
		done:    make(chan struct{}),
	}
	go c.read()
	return c, nil
}

// Browser returns the browser-level session.
func (c *Conn) Browser() *Session { return &Session{conn: c} }

// Session returns a handle for a flat session id from Target.attachToTarget.
func (c *Conn) Session(id string) *Session { return &Session{conn: c, id: id} }

// Done is closed when the connection ends; Err reports why.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) Close() error {
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	<-c.done
	return err
}

func (c *Conn) read() {
	var err error
	defer func() { c.shutdown(err) }()
	ctx := context.Background()
	for {
		var data []byte
		_, data, err = c.ws.Read(ctx)
		if err != nil {
			return
		}
		m := new(message)
		if uerr := jsonv2.Unmarshal(data, m); uerr != nil {
			continue // a malformed frame must not kill the connection
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		c.mu.Lock()
		for s := range c.subs[subKey{m.SessionID, m.Method}] {
			s.push(m.Params)
		}
		c.mu.Unlock()
	}
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = fmt.Errorf("%w: %v", ErrClosed, err)
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	for _, set := range c.subs {
		for s := range set {
			s.stop()
		}
	}
	c.subs = map[subKey]map[*subscription]struct{}{}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) call(ctx context.Context, session, method string, params, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	m := &message{ID: c.nextID.Add(1), SessionID: session, Method: method}
	if params != nil {
		b, err := jsonv2.Marshal(params)
		if err != nil {
			return fmt.Errorf("cdp: %s: encode params: %w", method, err)
		}
		m.Params = b
	}
	frame, err := jsonv2.Marshal(m)
	if err != nil {
		return err
	}

	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[m.ID] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
	}

	c.writeMu.Lock()
	err = c.ws.Write(ctx, websocket.MessageText, frame)
	c.writeMu.Unlock()
	if err != nil {
		forget()
		return fmt.Errorf("cdp: %s: write: %w", method, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if resp.Error != nil {
			resp.Error.Method = method
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := jsonv2.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("cdp: %s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		forget()
		return fmt.Errorf("cdp: %s: %w", method, ctx.Err())
	}
}

// Session is a flat CDP session. It implements cdproto's cdp.Session, so the
// generated commands work directly: cdp.Call(ctx, s, page.Navigate, params).
type Session struct {
	conn *Conn
	id   string
}

var _ cdproto.Session = (*Session)(nil)

func (s *Session) ID() string  { return s.id }
func (s *Session) Conn() *Conn { return s.conn }

func (s *Session) Call(ctx context.Context, method string, params, result any) error {
	return s.conn.call(ctx, s.id, method, params, result)
}

// Subscribe buffers events of method for this session without bound, so a
// slow consumer never blocks the reader or loses events.
func (s *Session) Subscribe(method string) (<-chan jsonv2.Value, func()) {
	sub := newSubscription()
	key := subKey{s.id, method}
	c := s.conn
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		sub.stop()
		return sub.out, func() {}
	}
	if c.subs[key] == nil {
		c.subs[key] = map[*subscription]struct{}{}
	}
	c.subs[key][sub] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return sub.out, func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs[key], sub)
			if len(c.subs[key]) == 0 {
				delete(c.subs, key)
			}
			c.mu.Unlock()
			sub.stop()
		})
	}
}

type subscription struct {
	mu     sync.Mutex
	queue  []jsonv2.Value
	wake   chan struct{}
	quit   chan struct{}
	out    chan jsonv2.Value
	closed bool
}

func newSubscription() *subscription {
	s := &subscription{
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		out:  make(chan jsonv2.Value),
	}
	go s.pump()
	return s
}

func (s *subscription) push(v jsonv2.Value) {
	s.mu.Lock()
	if !s.closed {
		s.queue = append(s.queue, v)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscription) stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.quit)
	}
	s.mu.Unlock()
}

func (s *subscription) pump() {
	defer close(s.out)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.quit:
				return
			}
		}
		v := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case s.out <- v:
		case <-s.quit:
			return
		}
	}
}
