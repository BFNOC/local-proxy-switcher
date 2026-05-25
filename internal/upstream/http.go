package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HTTPDialer 通过 HTTP CONNECT 上游代理打开目标连接。
type HTTPDialer struct {
	Upstream Upstream
	Timeout  time.Duration
}

// DialContext 通过配置的 HTTP 上游连接目标。
func (d HTTPDialer) DialContext(ctx context.Context, target Target) (net.Conn, error) {
	var dialer net.Dialer
	if d.Timeout > 0 {
		dialer.Timeout = d.Timeout
	}
	conn, err := dialer.DialContext(ctx, "tcp", d.Upstream.Address())
	if err != nil {
		return nil, err
	}
	if d.Timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(d.Timeout))
		defer conn.SetDeadline(time.Time{})
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target.Address()},
		Host:   target.Address(),
		Header: make(http.Header),
	}
	if d.Upstream.Username != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(d.Upstream.Username + ":" + d.Upstream.Password))
		req.Header.Set("Proxy-Authorization", "Basic "+encoded)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		conn.Close()
		return nil, fmt.Errorf("http upstream CONNECT failed: %s", resp.Status)
	}
	return conn, nil
}
