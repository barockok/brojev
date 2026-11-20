package page

// Stress harness for the Turnstile resolver. It is opt-in:
//
//	BROJEV_E2E=1 BROJEV_STRESS=1 go test ./internal/page/ -run 'TestTurnstileStress' -count=1 -v
//
// It uses Cloudflare's published test sitekeys on the local fixture server:
// 1x always passes, 2x always blocks, 3x forces the interactive widget. It
// never calls CapSolver or Jev, so a run has no external cost.

import (
	"context"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"

	"github.com/barockok/brojev/internal/turnstile"
)

func stressEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("BROJEV_STRESS") != "1" {
		t.Skip("set BROJEV_STRESS=1 to run the stress harness")
	}
}

func stressN(def int) int {
	if v := os.Getenv("TURNSTILE_STRESS_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

type stressTab struct {
	p    *Page
	id   target.ID
	ctx  context.Context
	stop context.CancelFunc
}

func openStressTab(url string) (*stressTab, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	info, err := shared.b.Open(ctx, url)
	if err != nil {
		cancel()
		return nil, err
	}
	id := target.ID(info.TargetID)
	tab, err := shared.b.Attach(ctx, id)
	if err != nil {
		shared.b.CloseTab(context.Background(), id)
		cancel()
		return nil, err
	}
	p, err := New(ctx, tab.ID, tab.Session)
	if err != nil {
		shared.b.CloseTab(context.Background(), id)
		cancel()
		return nil, err
	}
	return &stressTab{p: p, id: id, ctx: ctx, stop: cancel}, nil
}

func (s *stressTab) close() {
	s.p.Close()
	shared.b.CloseTab(context.Background(), s.id)
	s.stop()
}

func stressWait(ctx context.Context, p *Page) *turnstile.Status {
	deadline := time.Now().Add(20 * time.Second)
	var st *turnstile.Status
	for time.Now().Before(deadline) {
		st = p.DetectTurnstile(ctx)
		if st.Detected && (st.Mode == turnstile.ModeManaged || st.State == turnstile.StateSolved) {
			return st
		}
		if ctx.Err() != nil {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
	return p.DetectTurnstile(ctx)
}

// stressEnsure runs the production resolver without the opt-in external
// tiers, so the numbers measure the in-page path and failure bookkeeping.
func stressEnsure(ctx context.Context, p *Page, inPage time.Duration) *turnstile.Status {
	return p.ensureTurnstile(ctx, turnstileOptions{
		Total: 15 * time.Second, InPage: inPage, VisualBudget: 5 * time.Second,
	})
}

type stressStats struct {
	runs       int
	loadFailed int
	solved     int
	unresolved int
	absent     int
	error      int
	attempts   int
	times      []time.Duration
	reasons    map[string]int
}

func (s *stressStats) record(before, after *turnstile.Status, d time.Duration) {
	if s.reasons == nil {
		s.reasons = map[string]int{}
	}
	s.runs++
	s.times = append(s.times, d)
	if before == nil || !before.Detected {
		s.loadFailed++
	}
	if after == nil {
		s.error++
		return
	}
	s.attempts += after.Attempts
	switch after.State {
	case turnstile.StateSolved:
		s.solved++
	case turnstile.StateUnresolved:
		s.unresolved++
	case turnstile.StateAbsent:
		s.absent++
	}
	if after.Reason != "" {
		s.reasons[after.Reason]++
	}
}

func (s *stressStats) report(t *testing.T, label string, want turnstile.State) {
	t.Helper()
	if len(s.times) > 0 {
		sort.Slice(s.times, func(i, j int) bool { return s.times[i] < s.times[j] })
		var sum time.Duration
		for _, d := range s.times {
			sum += d
		}
		p95 := s.times[len(s.times)*95/100]
		t.Logf("%-20s runs=%d loadFail=%d solved=%d unresolved=%d absent=%d error=%d attempts=%d mean=%s p95=%s max=%s",
			label, s.runs, s.loadFailed, s.solved, s.unresolved, s.absent, s.error, s.attempts,
			(sum / time.Duration(len(s.times))).Round(time.Millisecond), p95.Round(time.Millisecond),
			s.times[len(s.times)-1].Round(time.Millisecond))
	}
	for reason, n := range s.reasons {
		t.Logf("%-20s reason x%d: %s", label, n, reason)
	}
	attempted := s.runs - s.loadFailed
	if attempted <= 0 {
		return
	}
	if want == turnstile.StateSolved && s.solved < attempted {
		t.Errorf("%s: solved %d of %d attempted", label, s.solved, attempted)
	}
	if want == turnstile.StateUnresolved && s.unresolved < attempted {
		t.Errorf("%s: unresolved %d of %d attempted", label, s.unresolved, attempted)
	}
}

func TestTurnstileStressSequential(t *testing.T) {
	stressEnabled(t)
	n := stressN(24)
	cases := []struct {
		path   string
		want   turnstile.State
		inPage time.Duration
	}{
		{"/turnstile-force", turnstile.StateSolved, 5 * time.Second},
		{"/turnstile-pass", turnstile.StateSolved, 5 * time.Second},
		{"/turnstile-block", turnstile.StateUnresolved, 500 * time.Millisecond},
		{"/turnstile-noframe", turnstile.StateSolved, 5 * time.Second},
	}
	stats := map[string]*stressStats{}
	for i := 0; i < n; i++ {
		c := cases[i%len(cases)]
		s, err := openStressTab(shared.base + c.path)
		if err != nil {
			t.Fatalf("open %s: %v", c.path, err)
		}
		before := stressWait(s.ctx, s.p)
		start := time.Now()
		after := stressEnsure(s.ctx, s.p, c.inPage)
		elapsed := time.Since(start)
		s.close()
		if stats[c.path] == nil {
			stats[c.path] = &stressStats{}
		}
		stats[c.path].record(before, after, elapsed)
	}
	for _, c := range cases {
		if st := stats[c.path]; st != nil {
			st.report(t, c.path, c.want)
		}
	}
}

func TestTurnstileStressParallel(t *testing.T) {
	stressEnabled(t)
	n := stressN(8)
	type result struct {
		before *turnstile.Status
		after  *turnstile.Status
		ms     int64
		err    error
	}
	out := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := openStressTab(shared.base + "/turnstile-force")
			if err != nil {
				out[i] = result{err: err}
				return
			}
			before := stressWait(s.ctx, s.p)
			start := time.Now()
			after := stressEnsure(s.ctx, s.p, 5*time.Second)
			out[i] = result{before: before, after: after, ms: time.Since(start).Milliseconds()}
			s.close()
		}(i)
	}
	wg.Wait()
	st := &stressStats{}
	var loadErr int
	for _, r := range out {
		if r.err != nil {
			loadErr++
			continue
		}
		st.record(r.before, r.after, time.Duration(r.ms)*time.Millisecond)
	}
	if loadErr > 0 {
		t.Logf("parallel open errors: %d", loadErr)
	}
	st.report(t, "parallel /force", turnstile.StateSolved)
}

func TestTurnstileStressDetectionOverhead(t *testing.T) {
	stressEnabled(t)
	n := stressN(50)
	s, err := openStressTab(shared.base + "/form")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	start := time.Now()
	for i := 0; i < n; i++ {
		if st := s.p.EnsureTurnstile(s.ctx); st.State != turnstile.StateAbsent {
			t.Fatalf("no-widget page reported %+v", st)
		}
	}
	mean := time.Since(start) / time.Duration(n)
	t.Logf("detection on a no-widget page: %d runs, mean %s", n, mean.Round(time.Microsecond))
}
