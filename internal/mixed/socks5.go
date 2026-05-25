package mixed

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

func (s *Server) handleSOCKS5(client net.Conn, br *bufio.Reader) error {
	if err := socks5Handshake(client, br); err != nil {
		return err
	}
	target, err := socks5ReadRequest(br)
	if err != nil {
		_, _ = client.Write(socks5Reply(0x01))
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.operationTimeout())
	defer cancel()
	remote, err := s.dial(ctx, target.Host, target.Port)
	if err != nil {
		_, _ = client.Write(socks5Reply(0x05))
		return err
	}
	if _, err := client.Write(socks5Reply(0x00)); err != nil {
		remote.Close()
		return err
	}
	pair := &connPair{a: client, b: remote}
	unregister := s.tracker.Register(pair)
	defer unregister()
	defer pair.Close()
	proxyBidirectional(client, remote)
	return nil
}

func socks5Handshake(w io.Writer, r io.Reader) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return fmt.Errorf("invalid SOCKS version 0x%02x", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(r, methods); err != nil {
		return err
	}
	for _, method := range methods {
		if method == 0x00 {
			_, err := w.Write([]byte{0x05, 0x00})
			return err
		}
	}
	_, _ = w.Write([]byte{0x05, 0xff})
	return fmt.Errorf("SOCKS5 client did not offer no-auth")
}

func socks5ReadRequest(r io.Reader) (upstream.Target, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return upstream.Target{}, err
	}
	if head[0] != 0x05 {
		return upstream.Target{}, fmt.Errorf("invalid SOCKS version 0x%02x", head[0])
	}
	if head[1] != 0x01 {
		return upstream.Target{}, fmt.Errorf("unsupported SOCKS5 command 0x%02x", head[1])
	}
	host, err := socks5ReadHost(r, head[3])
	if err != nil {
		return upstream.Target{}, err
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return upstream.Target{}, err
	}
	return upstream.Target{Host: host, Port: int(binary.BigEndian.Uint16(portBuf))}, nil
}

func socks5ReadHost(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", err
		}
		buf := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	default:
		return "", fmt.Errorf("unsupported SOCKS5 address type 0x%02x", atyp)
	}
}

func socks5Reply(rep byte) []byte {
	return []byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
}
