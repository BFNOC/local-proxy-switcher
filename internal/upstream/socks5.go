package upstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// SOCKS5Dialer 通过 SOCKS5 上游代理打开目标连接。
type SOCKS5Dialer struct {
	Upstream Upstream
	Timeout  time.Duration
}

// DialContext 通过配置的 SOCKS5 上游连接目标。
func (d SOCKS5Dialer) DialContext(ctx context.Context, target Target) (net.Conn, error) {
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
	if err := d.handshake(conn); err != nil {
		conn.Close()
		return nil, err
	}
	req, err := buildSOCKS5ConnectRequest(target)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		conn.Close()
		return nil, err
	}
	if header[0] != 0x05 {
		conn.Close()
		return nil, errors.New("invalid SOCKS5 upstream response")
	}
	if header[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 upstream connect failed: 0x%02x", header[1])
	}
	if err := discardSOCKS5Addr(conn, header[3]); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (d SOCKS5Dialer) handshake(conn net.Conn) error {
	if d.Upstream.Username == "" {
		if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
			return err
		}
		resp := make([]byte, 2)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return err
		}
		if resp[0] != 0x05 || resp[1] != 0x00 {
			return fmt.Errorf("SOCKS5 upstream rejected no-auth: 0x%02x", resp[1])
		}
		return nil
	}
	if _, err := conn.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] != 0x02 {
		return fmt.Errorf("SOCKS5 upstream rejected username/password auth: 0x%02x", resp[1])
	}
	user := []byte(d.Upstream.Username)
	pass := []byte(d.Upstream.Password)
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("SOCKS5 credentials too long")
	}
	authReq := []byte{0x01, byte(len(user))}
	authReq = append(authReq, user...)
	authReq = append(authReq, byte(len(pass)))
	authReq = append(authReq, pass...)
	if _, err := conn.Write(authReq); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 0x01 || resp[1] != 0x00 {
		return errors.New("SOCKS5 upstream authentication failed")
	}
	return nil
}

func buildSOCKS5ConnectRequest(target Target) ([]byte, error) {
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(target.Host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		host := []byte(target.Host)
		if len(host) > 255 {
			return nil, errors.New("SOCKS5 target domain too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, uint16(target.Port))
	req = append(req, port...)
	return req, nil
}

func discardSOCKS5Addr(r io.Reader, atyp byte) error {
	switch atyp {
	case 0x01:
		_, err := io.CopyN(io.Discard, r, 4+2)
		return err
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return err
		}
		_, err := io.CopyN(io.Discard, r, int64(lenBuf[0])+2)
		return err
	case 0x04:
		_, err := io.CopyN(io.Discard, r, 16+2)
		return err
	default:
		return fmt.Errorf("unsupported SOCKS5 address type 0x%02x", atyp)
	}
}
