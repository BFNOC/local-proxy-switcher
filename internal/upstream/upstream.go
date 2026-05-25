package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/redact"
)

// Upstream 描述一个当前可用的上游代理。
type Upstream struct {
	ID        string    `json:"id"`
	Scheme    string    `json:"scheme"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"-"`
	Password  string    `json:"-"`
	Net       string    `json:"net,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Source    string    `json:"source,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

// Target 描述本地代理客户端请求的最终目标地址。
type Target struct {
	Host string
	Port int
}

// Dialer 通过某种上游模式打开到目标的 TCP 连接。
type Dialer interface {
	DialContext(ctx context.Context, target Target) (net.Conn, error)
}

// Parse 将手动输入的上游 URL 解析为 Upstream。
func Parse(raw, defaultScheme string, defaultTTL time.Duration) (Upstream, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Upstream{}, errors.New("empty upstream URL")
	}
	if !strings.Contains(raw, "://") {
		if defaultScheme == "" {
			defaultScheme = "http"
		}
		raw = defaultScheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Upstream{}, err
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "socks5" && scheme != "socks5h" && scheme != "direct" {
		return Upstream{}, fmt.Errorf("unsupported upstream scheme %q", u.Scheme)
	}
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	if scheme == "direct" {
		return Upstream{
			ID:        "direct",
			Scheme:    "direct",
			Source:    "manual",
			FetchedAt: time.Now(),
		}, nil
	}
	host := u.Hostname()
	portText := u.Port()
	if host == "" || portText == "" {
		return Upstream{}, fmt.Errorf("upstream requires host and port: %s", redact.URL(raw))
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return Upstream{}, fmt.Errorf("invalid upstream port %q", portText)
	}
	username := u.User.Username()
	password, _ := u.User.Password()
	up := Upstream{
		Scheme:    scheme,
		Host:      host,
		Port:      port,
		Username:  username,
		Password:  password,
		Source:    "manual",
		ExpiresAt: expiry(defaultTTL),
		FetchedAt: time.Now(),
	}
	up.ID = up.BaseURL()
	return up, nil
}

func expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

// Address 返回用于网络拨号的 host:port。
func (u Upstream) Address() string {
	if u.Scheme == "direct" {
		return ""
	}
	return net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
}

// BaseURL 返回不含凭据的上游地址。
func (u Upstream) BaseURL() string {
	if u.Scheme == "direct" {
		return "direct://"
	}
	return u.Scheme + "://" + u.Address()
}

// RedactedURL 返回隐藏敏感信息后的上游地址。
func (u Upstream) RedactedURL() string {
	if u.Scheme == "direct" {
		return "direct://"
	}
	if u.Username == "" {
		return u.Scheme + "://" + u.Address()
	}
	return u.Scheme + "://" + url.UserPassword(u.Username, "***").String() + "@" + u.Address()
}

// IsExpired 判断上游在给定时间是否已经过期。
func (u Upstream) IsExpired(now time.Time) bool {
	return !u.ExpiresAt.IsZero() && !now.Before(u.ExpiresAt)
}

// Address 返回用于网络拨号的 host:port。
func (t Target) Address() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// ParseTarget 解析目标地址；缺少端口时使用 defaultPort。
func ParseTarget(addr, defaultPort string) (Target, error) {
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, defaultPort)
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return Target{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return Target{}, fmt.Errorf("invalid target port %q", portText)
	}
	return Target{Host: host, Port: port}, nil
}

// NewDialer 为上游创建对应协议的拨号器。
func NewDialer(up Upstream, timeout time.Duration) (Dialer, error) {
	switch up.Scheme {
	case "http":
		return HTTPDialer{Upstream: up, Timeout: timeout}, nil
	case "socks5":
		return SOCKS5Dialer{Upstream: up, Timeout: timeout}, nil
	case "direct":
		return DirectDialer{Timeout: timeout}, nil
	default:
		return nil, fmt.Errorf("unsupported upstream scheme %q", up.Scheme)
	}
}
