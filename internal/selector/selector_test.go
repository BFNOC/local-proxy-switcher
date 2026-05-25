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
