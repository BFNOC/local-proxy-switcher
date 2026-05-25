package mixed

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
)

// Options 配置 mixed 入站代理服务。
type Options struct {
	Addr             string
	Selector         *selector.Selector
	Tracker          *tracker.Tracker
	HandshakeTimeout time.Duration
}

// Server 在同一个 TCP 端口接受本地 HTTP 和 SOCKS5 代理流量。
type Server struct {
	addr             string
	selector         *selector.Selector
	tracker          *tracker.Tracker
	handshakeTimeout time.Duration
}

// NewServer 创建 mixed 代理服务。
func NewServer(opts Options) *Server {
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = 3 * time.Second
	}
	if opts.Tracker == nil {
		opts.Tracker = tracker.New()
	}
	return &Server{
		addr:             opts.Addr,
		selector:         opts.Selector,
		tracker:          opts.Tracker,
		handshakeTimeout: opts.HandshakeTimeout,
	}
}

// ListenAndServe 监听直到上下文取消或监听器失败。
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if s.handshakeTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.handshakeTimeout))
	}
	br := bufio.NewReader(conn)
	head, err := br.Peek(1)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	wrapped := &bufferedConn{Conn: conn, reader: br}
	if head[0] == 0x05 {
		if err := s.handleSOCKS5(wrapped, br); err != nil {
			return
		}
		return
	}
	if err := s.handleHTTP(wrapped, br); err != nil {
		return
	}
}

func (s *Server) dial(ctx context.Context, targetHost string, targetPort int) (net.Conn, error) {
	if s.selector == nil {
		return nil, fmt.Errorf("selector is not configured")
	}
	return s.selector.Dial(ctx, targetFromParts(targetHost, targetPort))
}

func (s *Server) operationTimeout() time.Duration {
	if s.handshakeTimeout > 0 {
		return s.handshakeTimeout
	}
	return 3 * time.Second
}
