package upstream

import (
	"context"
	"net"
	"time"
)

// DirectDialer 不经过上游代理直接连接目标。
type DirectDialer struct {
	Timeout time.Duration
}

// DialContext 直接连接目标。
func (d DirectDialer) DialContext(ctx context.Context, target Target) (net.Conn, error) {
	var dialer net.Dialer
	if d.Timeout > 0 {
		dialer.Timeout = d.Timeout
	}
	return dialer.DialContext(ctx, "tcp", target.Address())
}
