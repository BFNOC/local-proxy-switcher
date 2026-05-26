package selector

import (
	"testing"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func TestSwitchHistoryIsCapped(t *testing.T) {
	s := New(Options{HistoryLimit: 2})
	for i := 0; i < 4; i++ {
		s.Switch(upstream.Upstream{
			Scheme:    "http",
			Host:      "127.0.0.1",
			Port:      8000 + i,
			ExpiresAt: time.Now().Add(time.Minute),
		}, false, "test")
	}
	history := s.History()
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	if history[0].To != "http://127.0.0.1:8002" || history[1].To != "http://127.0.0.1:8003" {
		t.Fatalf("unexpected history: %+v", history)
	}
}

func TestSwitchKeepsCurrentWithoutInterrupt(t *testing.T) {
	s := New(Options{})
	up := upstream.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8080, ExpiresAt: time.Now().Add(time.Minute)}
	s.Switch(up, false, "test")
	cur, ok := s.Current()
	if !ok {
		t.Fatal("missing current upstream")
	}
	if cur.RedactedURL() != "http://127.0.0.1:8080" {
		t.Fatalf("current = %s", cur.RedactedURL())
	}
}

func TestClearRemovesCurrent(t *testing.T) {
	s := New(Options{})
	up := upstream.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8080, ExpiresAt: time.Now().Add(time.Minute)}
	s.Switch(up, false, "test")
	s.Clear(false, "test clear")
	if _, ok := s.Current(); ok {
		t.Fatal("current upstream was not cleared")
	}
}

func TestWatchNotifiesSwitchAndClear(t *testing.T) {
	s := New(Options{})
	watch := s.Watch()
	up := upstream.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8080, ExpiresAt: time.Now().Add(time.Minute)}

	s.Switch(up, false, "test")
	expectNotify(t, watch)
	s.Clear(false, "test clear")
	expectNotify(t, watch)
}

func TestSwitchProviderRefreshRespectsManualState(t *testing.T) {
	providerUp := upstream.Upstream{Scheme: "http", Host: "203.0.113.10", Port: 8080, Source: "provider", ExpiresAt: time.Now().Add(time.Minute)}
	manualUp := upstream.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8080, Source: "manual", ExpiresAt: time.Now().Add(time.Minute)}

	s := New(Options{})
	_, _, _, version := s.ProviderRefreshSnapshot()
	if !s.SwitchProviderRefresh(providerUp, false, "auto refresh", version) {
		t.Fatal("provider refresh should initialize an empty selector")
	}

	s.Switch(manualUp, false, "manual lock")
	_, _, _, version = s.ProviderRefreshSnapshot()
	if s.SwitchProviderRefresh(providerUp, false, "auto refresh", version) {
		t.Fatal("provider refresh overwrote manual lock")
	}
	cur, ok, paused, _ := s.ProviderRefreshSnapshot()
	if !ok || cur.Source != "manual" || paused {
		t.Fatalf("snapshot = %+v ok=%v paused=%v", cur, ok, paused)
	}

	s.Clear(false, "manual clear")
	_, _, _, version = s.ProviderRefreshSnapshot()
	if s.SwitchProviderRefresh(providerUp, false, "auto refresh", version) {
		t.Fatal("provider refresh overwrote manual clear")
	}
	if _, ok, paused, _ := s.ProviderRefreshSnapshot(); ok || !paused {
		t.Fatalf("snapshot after clear ok=%v paused=%v", ok, paused)
	}
}

func TestSwitchProviderRefreshRejectsStaleVersion(t *testing.T) {
	s := New(Options{})
	providerUp := upstream.Upstream{Scheme: "http", Host: "203.0.113.10", Port: 8080, Source: "provider", ExpiresAt: time.Now().Add(time.Minute)}
	otherProvider := upstream.Upstream{Scheme: "http", Host: "203.0.113.11", Port: 8080, Source: "provider", ExpiresAt: time.Now().Add(time.Minute)}

	_, _, _, staleVersion := s.ProviderRefreshSnapshot()
	s.Switch(otherProvider, false, "provider switch")
	if s.SwitchProviderRefresh(providerUp, false, "auto refresh", staleVersion) {
		t.Fatal("stale provider refresh overwrote newer provider switch")
	}
	cur, ok := s.Current()
	if !ok || cur.Host != "203.0.113.11" {
		t.Fatalf("current = %+v ok=%v", cur, ok)
	}
}

func TestUnwatchStopsNotifications(t *testing.T) {
	s := New(Options{})
	watch := s.Watch()
	s.Unwatch(watch)

	up := upstream.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8080, ExpiresAt: time.Now().Add(time.Minute)}
	s.Switch(up, false, "test")
	expectNoNotify(t, watch)
}

func expectNotify(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for selector notification")
	}
}

func expectNoNotify(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected selector notification")
	case <-time.After(20 * time.Millisecond):
	}
}
