package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func TestParseTCPingTargetHTTPS(t *testing.T) {
	srv := NewServer(Options{
		Selector:       selector.New(selector.Options{}),
		TCPingResolver: fakeTCPingResolver{"example.com": ips("93.184.216.34")},
	})

	target, err := srv.parseTCPingTarget(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("parseTCPingTarget returned error: %v", err)
	}
	if target.URL != "https://example.com" {
		t.Fatalf("URL = %q, want https://example.com", target.URL)
	}
	if target.DisplayTarget != "example.com:443" {
		t.Fatalf("DisplayTarget = %q, want example.com:443", target.DisplayTarget)
	}
	if target.Target.Host != "example.com" || target.Target.Port != 443 {
		t.Fatalf("Target = %+v, want example.com:443", target.Target)
	}
}

func TestParseTCPingTargetDefaultsBareHostToHTTPS(t *testing.T) {
	srv := NewServer(Options{
		Selector:       selector.New(selector.Options{}),
		TCPingResolver: fakeTCPingResolver{"example.com": ips("93.184.216.34")},
	})

	target, err := srv.parseTCPingTarget(context.Background(), "example.com:8443")
	if err != nil {
		t.Fatalf("parseTCPingTarget returned error: %v", err)
	}
	if target.URL != "https://example.com:8443" {
		t.Fatalf("URL = %q, want https://example.com:8443", target.URL)
	}
	if target.Target.Port != 8443 {
		t.Fatalf("Port = %d, want 8443", target.Target.Port)
	}
}

func TestParseTCPingTargetRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "http scheme", raw: "http://example.com"},
		{name: "credentials", raw: "https://user:pass@example.com"},
		{name: "localhost", raw: "https://localhost"},
		{name: "local suffix", raw: "https://printer.local"},
		{name: "ip literal", raw: "https://93.184.216.34"},
		{name: "unsupported port", raw: "https://example.com:22"},
	}
	srv := NewServer(Options{
		Selector:       selector.New(selector.Options{}),
		TCPingResolver: fakeTCPingResolver{"example.com": ips("93.184.216.34")},
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := srv.parseTCPingTarget(context.Background(), tt.raw); err == nil {
				t.Fatal("parseTCPingTarget returned nil error")
			}
		})
	}
}

func TestParseTCPingTargetRejectsUnsafeResolvedIP(t *testing.T) {
	srv := NewServer(Options{
		Selector:       selector.New(selector.Options{}),
		TCPingResolver: fakeTCPingResolver{"example.com": ips("10.0.0.1")},
	})

	if _, err := srv.parseTCPingTarget(context.Background(), "https://example.com"); err == nil {
		t.Fatal("parseTCPingTarget returned nil error")
	}
}

func TestHandleTCPingMethodNotAllowed(t *testing.T) {
	srv := newTCPingTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:17990/tcping", nil)
	w := httptest.NewRecorder()

	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleTCPingRejectsCrossOrigin(t *testing.T) {
	srv := newTCPingTestServer(t, nil)
	body := bytes.NewBufferString(`{"url":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/tcping", body)
	req.Host = "127.0.0.1:17990"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	w := httptest.NewRecorder()

	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHandleTCPingNoCurrentUpstream(t *testing.T) {
	sel := selector.New(selector.Options{})
	srv := NewServer(Options{
		Selector:       sel,
		TCPingResolver: fakeTCPingResolver{"example.com": ips("93.184.216.34")},
		TCPingDial:     func(context.Context, upstream.Target) (net.Conn, error) { return nil, errors.New("unexpected dial") },
	})
	body := bytes.NewBufferString(`{"url":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/tcping", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(w.Body.String(), "no current upstream") {
		t.Fatalf("body = %s, want no current upstream", w.Body.String())
	}
}

func TestHandleTCPingSuccessClampsCount(t *testing.T) {
	var calls int
	var targets []upstream.Target
	srv := newTCPingTestServer(t, func(_ context.Context, target upstream.Target) (net.Conn, error) {
		calls++
		targets = append(targets, target)
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	})
	body := bytes.NewBufferString(`{"url":"https://example.com","count":99}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/tcping", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp TCPingResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || resp.Loss != 0 {
		t.Fatalf("response = %+v, want success with no loss", resp)
	}
	if resp.Count != maxTCPingCount || len(resp.Probes) != maxTCPingCount || calls != maxTCPingCount {
		t.Fatalf("count=%d probes=%d calls=%d, want %d", resp.Count, len(resp.Probes), calls, maxTCPingCount)
	}
	for _, target := range targets {
		if target.Host != "example.com" || target.Port != 443 {
			t.Fatalf("dial target = %+v, want example.com:443", target)
		}
	}
	if resp.Via != "direct://" {
		t.Fatalf("Via = %q, want direct://", resp.Via)
	}
}

func TestHandleTCPingProbeFailure(t *testing.T) {
	srv := newTCPingTestServer(t, func(context.Context, upstream.Target) (net.Conn, error) {
		return nil, errors.New("dial failed")
	})
	body := bytes.NewBufferString(`{"url":"https://example.com","count":1}`)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17990/tcping", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp TCPingResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success || resp.Loss != 1 || resp.Error == "" {
		t.Fatalf("response = %+v, want failed probe result", resp)
	}
}

func newTCPingTestServer(t *testing.T, dial func(context.Context, upstream.Target) (net.Conn, error)) *Server {
	t.Helper()
	sel := selector.New(selector.Options{})
	sel.Switch(upstream.Upstream{Scheme: "direct", Source: "manual"}, false, "test")
	if dial == nil {
		dial = func(context.Context, upstream.Target) (net.Conn, error) {
			return nil, errors.New("unexpected dial")
		}
	}
	return NewServer(Options{
		Selector:       sel,
		TCPingResolver: fakeTCPingResolver{"example.com": ips("93.184.216.34")},
		TCPingDial:     dial,
	})
}

type fakeTCPingResolver map[string][]net.IPAddr

func (r fakeTCPingResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, fmt.Errorf("unexpected lookup host %q", host)
	}
	return addrs, nil
}

func ips(values ...string) []net.IPAddr {
	addrs := make([]net.IPAddr, 0, len(values))
	for _, value := range values {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(value)})
	}
	return addrs
}
