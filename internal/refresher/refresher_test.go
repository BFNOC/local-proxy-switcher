package refresher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func TestRunFetchesImmediatelyWhenCurrentIsEmpty(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	up := providerUpstream("203.0.113.10", now, now.Add(time.Minute))
	up.Source = ""
	fetcher := &fakeFetcher{
		up: up,
	}
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitCalls(t, 1)
	cur, ok := sel.Current()
	if !ok {
		t.Fatal("current upstream was not set")
	}
	if cur.Host != "203.0.113.10" || cur.Source != "provider" {
		t.Fatalf("current = %+v", cur)
	}
}

func TestRunPausesAfterManualClear(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	sel.Switch(providerUpstream("203.0.113.10", now, now.Add(time.Minute)), false, "provider switch")
	sel.Clear(false, "manual clear")
	fetcher := &fakeFetcher{
		up: providerUpstream("203.0.113.11", now, now.Add(time.Minute)),
	}
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.expectNoCalls(t, 20*time.Millisecond)
}

func TestRunPausesForManualUpstream(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	sel.Switch(upstream.Upstream{
		Scheme:    "http",
		Host:      "127.0.0.1",
		Port:      8080,
		Source:    "manual",
		FetchedAt: now,
		ExpiresAt: now.Add(time.Minute),
	}, false, "manual lock")
	fetcher := &fakeFetcher{
		up: providerUpstream("203.0.113.11", now, now.Add(time.Minute)),
	}
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.expectNoCalls(t, 20*time.Millisecond)
}

func TestManualProviderSwitchWakesPausedRun(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	sel.Switch(upstream.Upstream{
		Scheme:    "http",
		Host:      "127.0.0.1",
		Port:      8080,
		Source:    "manual",
		FetchedAt: now,
		ExpiresAt: now.Add(time.Minute),
	}, false, "manual lock")
	fetcher := &fakeFetcher{
		up: providerUpstream("203.0.113.11", now, now.Add(time.Minute)),
	}
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.expectNoCalls(t, 20*time.Millisecond)
	sel.Switch(providerUpstream("203.0.113.10", now.Add(-time.Minute), now), false, "provider switch")
	fetcher.waitCalls(t, 1)
}

func TestNextDelayRefreshesBeforeProviderExpiry(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	sel.Switch(providerUpstream("203.0.113.10", now, now.Add(10*time.Minute)), false, "provider switch")
	ref := New(Options{Selector: sel})

	delay, ok, _ := ref.nextDelay(now, false, 0)
	if !ok {
		t.Fatal("refresh was not scheduled")
	}
	if delay != 9*time.Minute+30*time.Second {
		t.Fatalf("delay = %v, want 9m30s", delay)
	}
}

func TestRunFailureKeepsCurrentAndRecordsError(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	original := providerUpstream("203.0.113.10", now.Add(-time.Minute), now)
	sel.Switch(original, false, "provider switch")
	fetcher := &fakeFetcher{err: errors.New("boom")}
	ref := New(Options{
		Fetcher:       fetcher,
		Selector:      sel,
		MinRetryDelay: time.Hour,
		MaxRetryDelay: time.Hour,
		Now:           func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitCalls(t, 1)
	waitUntil(t, func() bool {
		return strings.Contains(sel.LastError(), "auto refresh failed: boom")
	})
	cur, ok := sel.Current()
	if !ok || cur.Host != original.Host {
		t.Fatalf("current changed after failed refresh: %+v ok=%v", cur, ok)
	}
}

func TestRunRejectsExpiredFetchedUpstream(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	fetcher := &fakeFetcher{
		up: providerUpstream("203.0.113.10", now.Add(-time.Minute), now.Add(-time.Second)),
	}
	ref := New(Options{
		Fetcher:       fetcher,
		Selector:      sel,
		MinRetryDelay: time.Hour,
		MaxRetryDelay: time.Hour,
		Now:           func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitCalls(t, 1)
	waitUntil(t, func() bool {
		return strings.Contains(sel.LastError(), "provider returned expired upstream")
	})
	fetcher.expectCalls(t, 1, 20*time.Millisecond)
	if _, ok := sel.Current(); ok {
		t.Fatal("expired provider upstream was applied")
	}
}

func TestRunRejectsNearExpiredFetchedUpstream(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	fetcher := &fakeFetcher{
		up: providerUpstream("203.0.113.10", now, now.Add(time.Second)),
	}
	ref := New(Options{
		Fetcher:       fetcher,
		Selector:      sel,
		MinRetryDelay: time.Hour,
		MaxRetryDelay: time.Hour,
		Now:           func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitCalls(t, 1)
	waitUntil(t, func() bool {
		return strings.Contains(sel.LastError(), "provider returned near-expired upstream")
	})
	fetcher.expectCalls(t, 1, 20*time.Millisecond)
	if _, ok := sel.Current(); ok {
		t.Fatal("near-expired provider upstream was applied")
	}
}

func TestRunDoesNotRecordStaleErrorAfterManualLockDuringFetch(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	fetcher := newBlockingFetcher(upstream.Upstream{})
	fetcher.err = errors.New("boom")
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitStarted(t)
	sel.Switch(upstream.Upstream{
		Scheme:    "http",
		Host:      "127.0.0.1",
		Port:      8080,
		Source:    "manual",
		FetchedAt: now,
		ExpiresAt: now.Add(time.Minute),
	}, false, "manual lock")
	fetcher.release()
	fetcher.waitCalls(t, 1)
	fetcher.expectCalls(t, 1, 20*time.Millisecond)
	if got := sel.LastError(); got != "" {
		t.Fatalf("stale auto-refresh error was recorded: %q", got)
	}
}

func TestRunDoesNotOverwriteManualClearDuringFetch(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	fetcher := newBlockingFetcher(providerUpstream("203.0.113.10", now, now.Add(time.Minute)))
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitStarted(t)
	sel.Clear(false, "manual clear")
	fetcher.release()
	waitUntil(t, func() bool {
		fetcher.mu.Lock()
		defer fetcher.mu.Unlock()
		return fetcher.calls >= 1
	})
	if _, ok := sel.Current(); ok {
		t.Fatal("auto refresh overwrote manual clear")
	}
}

func TestRunDoesNotOverwriteManualLockDuringFetch(t *testing.T) {
	now := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	sel := selector.New(selector.Options{})
	fetcher := newBlockingFetcher(providerUpstream("203.0.113.10", now, now.Add(time.Minute)))
	ref := New(Options{Fetcher: fetcher, Selector: sel, Now: func() time.Time { return now }})

	ctx, cancel := context.WithCancel(context.Background())
	done := runRefresher(ctx, ref)
	defer stopRefresher(t, cancel, done)

	fetcher.waitStarted(t)
	sel.Switch(upstream.Upstream{
		Scheme:    "http",
		Host:      "127.0.0.1",
		Port:      8080,
		Source:    "manual",
		FetchedAt: now,
		ExpiresAt: now.Add(time.Minute),
	}, false, "manual lock")
	fetcher.release()
	waitUntil(t, func() bool {
		fetcher.mu.Lock()
		defer fetcher.mu.Unlock()
		return fetcher.calls >= 1
	})
	cur, ok := sel.Current()
	if !ok || cur.Host != "127.0.0.1" || cur.Source != "manual" {
		t.Fatalf("auto refresh overwrote manual lock: %+v ok=%v", cur, ok)
	}
}

type fakeFetcher struct {
	mu    sync.Mutex
	up    upstream.Upstream
	err   error
	calls int
	start chan struct{}
	block chan struct{}
}

func (f *fakeFetcher) Fetch(context.Context) (upstream.Upstream, error) {
	if f.start != nil {
		select {
		case f.start <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return upstream.Upstream{}, f.err
	}
	return f.up, nil
}

func newBlockingFetcher(up upstream.Upstream) *fakeFetcher {
	return &fakeFetcher{
		up:    up,
		start: make(chan struct{}, 1),
		block: make(chan struct{}),
	}
}

func (f *fakeFetcher) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.start:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
}

func (f *fakeFetcher) release() {
	close(f.block)
}

func (f *fakeFetcher) waitCalls(t *testing.T, want int) {
	t.Helper()
	waitUntil(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.calls >= want
	})
}

func (f *fakeFetcher) expectNoCalls(t *testing.T, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0", f.calls)
	}
}

func (f *fakeFetcher) expectCalls(t *testing.T, want int, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != want {
		t.Fatalf("fetch calls = %d, want %d", f.calls, want)
	}
}

func providerUpstream(host string, fetchedAt, expiresAt time.Time) upstream.Upstream {
	return upstream.Upstream{
		Scheme:    "http",
		Host:      host,
		Port:      8080,
		Source:    "provider",
		FetchedAt: fetchedAt,
		ExpiresAt: expiresAt,
		ID:        "http://" + host + ":8080",
	}
}

func runRefresher(ctx context.Context, ref *Refresher) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ref.Run(ctx)
	}()
	return done
}

func stopRefresher(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop")
	}
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition was not met before timeout")
		case <-tick.C:
		}
	}
}
