package mixed

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func (s *Server) handleHTTP(client net.Conn, br *bufio.Reader) error {
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if req.Method == http.MethodConnect {
			return s.handleHTTPConnect(client, req)
		}
		if err := s.handlePlainHTTP(client, req); err != nil {
			return err
		}
		if req.Close {
			return nil
		}
	}
}

func (s *Server) handleHTTPConnect(client net.Conn, req *http.Request) error {
	target, err := upstream.ParseTarget(req.Host, "443")
	if err != nil {
		writeHTTPError(client, http.StatusBadRequest, err.Error())
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.operationTimeout())
	defer cancel()
	remote, err := s.dial(ctx, target.Host, target.Port)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway, "连接目标失败")
		return err
	}
	pair := &connPair{a: client, b: remote}
	unregister := s.tracker.Register(pair)
	defer unregister()
	defer pair.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return err
	}
	proxyBidirectional(client, remote)
	return nil
}

func (s *Server) handlePlainHTTP(client net.Conn, req *http.Request) error {
	target, err := targetForHTTPRequest(req)
	if err != nil {
		writeHTTPError(client, http.StatusBadRequest, err.Error())
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.operationTimeout())
	defer cancel()
	remote, err := s.dial(ctx, target.Host, target.Port)
	if err != nil {
		writeHTTPError(client, http.StatusBadGateway, "连接目标失败")
		return err
	}
	pair := &connPair{a: client, b: remote}
	unregister := s.tracker.Register(pair)
	defer unregister()
	defer pair.Close()

	req.RequestURI = ""
	if req.URL != nil && req.URL.IsAbs() {
		req.URL.Scheme = ""
		req.URL.Host = ""
	}
	removeProxyHeaders(req.Header)
	req.Close = true
	if err := req.Write(remote); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(remote), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	resp.Close = true
	resp.Header.Set("Connection", "close")
	return resp.Write(client)
}

func targetForHTTPRequest(req *http.Request) (upstream.Target, error) {
	host := req.Host
	if req.URL != nil && req.URL.Host != "" {
		host = req.URL.Host
	}
	if host == "" {
		return upstream.Target{}, fmt.Errorf("missing Host")
	}
	defaultPort := "80"
	if req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") {
		defaultPort = "443"
	}
	return upstream.ParseTarget(host, defaultPort)
}

func writeHTTPError(conn net.Conn, status int, msg string) {
	text := http.StatusText(status)
	if text == "" {
		text = "Proxy Error"
	}
	body := text
	if msg != "" {
		body += ": " + msg
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", status, text, len(body), body)
}

func removeProxyHeaders(h http.Header) {
	for _, key := range []string{
		"Proxy-Authorization",
		"Proxy-Connection",
		"Proxy-Authenticate",
		"Connection",
	} {
		h.Del(key)
	}
}
