package upstream

import (
	"io"
	"net"
	"testing"
)

func TestSOCKS5HandshakeWithCredentialsOnlyOffersUserPass(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		head := make([]byte, 2)
		if _, err := io.ReadFull(server, head); err != nil {
			done <- err
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(server, methods); err != nil {
			done <- err
			return
		}
		if head[0] != 0x05 || head[1] != 0x01 || methods[0] != 0x02 {
			done <- io.ErrUnexpectedEOF
			return
		}
		if _, err := server.Write([]byte{0x05, 0x02}); err != nil {
			done <- err
			return
		}
		authHead := make([]byte, 2)
		if _, err := io.ReadFull(server, authHead); err != nil {
			done <- err
			return
		}
		user := make([]byte, int(authHead[1]))
		if _, err := io.ReadFull(server, user); err != nil {
			done <- err
			return
		}
		passLen := make([]byte, 1)
		if _, err := io.ReadFull(server, passLen); err != nil {
			done <- err
			return
		}
		pass := make([]byte, int(passLen[0]))
		if _, err := io.ReadFull(server, pass); err != nil {
			done <- err
			return
		}
		if string(user) != "user" || string(pass) != "pass" {
			done <- io.ErrUnexpectedEOF
			return
		}
		_, err := server.Write([]byte{0x01, 0x00})
		done <- err
	}()

	dialer := SOCKS5Dialer{Upstream: Upstream{Username: "user", Password: "pass"}}
	if err := dialer.handshake(client); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
