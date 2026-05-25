package mixed

import (
	"io"
	"net"
	"sync"
	"time"
)

type bufferedConn struct {
	net.Conn
	reader interface {
		Read([]byte) (int, error)
	}
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type connPair struct {
	a    net.Conn
	b    net.Conn
	once sync.Once
}

func (p *connPair) Close() error {
	var err error
	p.once.Do(func() {
		if p.a != nil {
			err = p.a.Close()
		}
		if p.b != nil {
			if e := p.b.Close(); err == nil {
				err = e
			}
		}
	})
	return err
}

func proxyBidirectional(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if buffered, ok := conn.(*bufferedConn); ok {
		closeWrite(buffered.Conn)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
		return
	}
	_ = conn.SetDeadline(time.Now())
}
