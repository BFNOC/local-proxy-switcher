package mixed

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func TestHTTPProxyThroughMixedPortAndHTTPUpstream(t *testing.T) {
	target := startHTTPOrigin(t)
	upstreamAddr := startHTTPConnectProxy(t)
	mixedAddr, cancel := startMixed(t, upstream.Upstream{
		Scheme:    "http",
		Host:      hostOf(upstreamAddr),
		Port:      portOf(t, upstreamAddr),
		ExpiresAt: time.Now().Add(time.Minute),
	})
	defer cancel()

	proxyURL, _ := url.Parse("http://" + mixedAddr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://" + target + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestSOCKS5DomainIsPassedToSOCKS5Upstream(t *testing.T) {
	recorded := make(chan string, 1)
	upstreamAddr := startRecordingSOCKS5Upstream(t, recorded)
	mixedAddr, cancel := startMixed(t, upstream.Upstream{
		Scheme:    "socks5",
		Host:      hostOf(upstreamAddr),
		Port:      portOf(t, upstreamAddr),
		ExpiresAt: time.Now().Add(time.Minute),
	})
	defer cancel()

	conn, err := net.Dial("tcp", mixedAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	if resp[1] != 0x00 {
		t.Fatalf("auth response = %v", resp)
	}
	host := []byte("example.test")
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, 443)
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("connect response = %v", reply)
	}
	select {
	case got := <-recorded:
		if got != "example.test:443" {
			t.Fatalf("recorded = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recorded target")
	}
}

func startMixed(t *testing.T, up upstream.Upstream) (string, context.CancelFunc) {
	t.Helper()
	addr := freeAddr(t)
	tr := tracker.New()
	sel := selector.New(selector.Options{Tracker: tr, DialTimeout: time.Second, FailClosedOnExpired: true})
	sel.Switch(up, false, "test")
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(Options{Addr: addr, Selector: sel, Tracker: tr, HandshakeTimeout: time.Second})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()
	waitForTCP(t, addr)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("mixed server: %v", err)
			}
		case <-time.After(time.Second):
		}
	})
	return addr, cancel
}

func startHTTPOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return ln.Addr().String()
}

func startHTTPConnectProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleHTTPConnectProxyConn(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func handleHTTPConnectProxyConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	target, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer target.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(target, br) }()
	go func() { defer wg.Done(); _, _ = io.Copy(conn, target) }()
	wg.Wait()
}

func startRecordingSOCKS5Upstream(t *testing.T, recorded chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleRecordingSOCKS5(conn, recorded)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func handleRecordingSOCKS5(conn net.Conn, recorded chan<- string) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	_, _ = conn.Write([]byte{0x05, 0x00})
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	host, err := socks5ReadHost(conn, req[3])
	if err != nil {
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	recorded <- fmt.Sprintf("%s:%d", host, binary.BigEndian.Uint16(portBuf))
	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not listen on %s", addr)
}

func hostOf(addr string) string {
	host, _, _ := net.SplitHostPort(addr)
	return host
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var out int
	if _, err := fmt.Sscanf(port, "%d", &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTargetForHTTPRequestRejectsMissingHost(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", strings.NewReader(""))
	req.Host = ""
	if _, err := targetForHTTPRequest(req); err == nil {
		t.Fatal("expected error")
	}
}
