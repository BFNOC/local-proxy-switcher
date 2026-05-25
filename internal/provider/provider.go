package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/redact"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

// Options 配置上游 provider 拉取器。
type Options struct {
	URL           string
	Timeout       time.Duration
	DefaultScheme string
	DefaultTTL    time.Duration
	HTTPClient    *http.Client
}

// Fetcher 拉取并解析短效上游代理。
type Fetcher struct {
	url           string
	timeout       time.Duration
	defaultScheme string
	defaultTTL    time.Duration
	client        *http.Client
}

// NewFetcher 使用安全默认值创建 provider 拉取器。
func NewFetcher(opts Options) *Fetcher {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.DefaultScheme == "" {
		opts.DefaultScheme = "http"
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = 10 * time.Minute
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	return &Fetcher{
		url:           opts.URL,
		timeout:       opts.Timeout,
		defaultScheme: opts.DefaultScheme,
		defaultTTL:    opts.DefaultTTL,
		client:        opts.HTTPClient,
	}
}

// Fetch 从 provider 获取一个上游代理。
func (f *Fetcher) Fetch(ctx context.Context) (upstream.Upstream, error) {
	if f == nil || strings.TrimSpace(f.url) == "" {
		return upstream.Upstream{}, errors.New("provider is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return upstream.Upstream{}, fmt.Errorf("provider request failed: %s", safeProviderError(err, f.url))
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return upstream.Upstream{}, fmt.Errorf("provider request failed: %s", safeProviderError(err, f.url))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return upstream.Upstream{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstream.Upstream{}, fmt.Errorf("provider %s returned %s", redact.URL(f.url), resp.Status)
	}
	serverNow := time.Now()
	if date := resp.Header.Get("Date"); date != "" {
		if parsed, err := http.ParseTime(date); err == nil {
			serverNow = parsed
		}
	}
	up, err := ParseResponse(body, ParseOptions{
		DefaultScheme: f.defaultScheme,
		DefaultTTL:    f.defaultTTL,
		ServerNow:     serverNow,
		LocalNow:      time.Now(),
	})
	if err != nil {
		return upstream.Upstream{}, err
	}
	up.Source = "provider"
	if up.FetchedAt.IsZero() {
		up.FetchedAt = time.Now()
	}
	if up.ID == "" {
		up.ID = fmt.Sprintf("%s@%d", up.BaseURL(), up.FetchedAt.Unix())
	}
	return up, nil
}

func safeProviderError(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), rawURL, redact.URL(rawURL))
}

// ParseOptions 控制 provider 响应解析。
type ParseOptions struct {
	DefaultScheme string
	DefaultTTL    time.Duration
	ServerNow     time.Time
	LocalNow      time.Time
}

type ipzanResponse struct {
	Data struct {
		List []ipzanNode `json:"list"`
	} `json:"data"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type ipzanNode struct {
	IP      string `json:"ip"`
	Port    int    `json:"port"`
	Expired int64  `json:"expired"`
	Net     string `json:"net"`
}

// ParseResponse 解析 IPZAN JSON 或普通 ip:port 响应。
func ParseResponse(body []byte, opts ParseOptions) (upstream.Upstream, error) {
	if opts.DefaultScheme == "" {
		opts.DefaultScheme = "http"
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = 10 * time.Minute
	}
	if opts.LocalNow.IsZero() {
		opts.LocalNow = time.Now()
	}
	if opts.ServerNow.IsZero() {
		opts.ServerNow = opts.LocalNow
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return upstream.Upstream{}, errors.New("provider returned empty response")
	}
	if strings.HasPrefix(trimmed, "{") {
		return parseJSON([]byte(trimmed), opts)
	}
	return parsePlain(trimmed, opts)
}

func parseJSON(body []byte, opts ParseOptions) (upstream.Upstream, error) {
	var resp ipzanResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return upstream.Upstream{}, err
	}
	if resp.Code != 0 || (resp.Status != 0 && resp.Status != 200) {
		return upstream.Upstream{}, fmt.Errorf("provider code=%d status=%d message=%s", resp.Code, resp.Status, resp.Message)
	}
	if len(resp.Data.List) == 0 {
		return upstream.Upstream{}, errors.New("provider returned no proxies")
	}
	node := resp.Data.List[0]
	if node.IP == "" || node.Port <= 0 {
		return upstream.Upstream{}, errors.New("provider returned invalid proxy")
	}
	up, err := upstream.Parse(fmt.Sprintf("%s://%s:%d", opts.DefaultScheme, node.IP, node.Port), opts.DefaultScheme, opts.DefaultTTL)
	if err != nil {
		return upstream.Upstream{}, err
	}
	up.Net = node.Net
	up.Source = "provider"
	up.FetchedAt = opts.LocalNow
	up.ExpiresAt = expiryFromProvider(node.Expired, opts)
	up.ID = fmt.Sprintf("%s@%d", up.BaseURL(), up.FetchedAt.Unix())
	return up, nil
}

func parsePlain(body string, opts ParseOptions) (upstream.Upstream, error) {
	line := strings.TrimSpace(strings.Split(body, "\n")[0])
	if line == "" {
		return upstream.Upstream{}, errors.New("provider returned no plain proxy")
	}
	// 有些 provider 在省略 format 后仍可能返回状态文本；非 host:port
	// 内容必须拒绝，避免悄悄锁定错误数据。
	host, portText, ok := strings.Cut(line, ":")
	if !ok || strings.TrimSpace(host) == "" {
		return upstream.Upstream{}, fmt.Errorf("invalid plain provider response %q", line)
	}
	if _, err := strconv.Atoi(strings.TrimSpace(portText)); err != nil {
		return upstream.Upstream{}, fmt.Errorf("invalid plain provider port %q", portText)
	}
	up, err := upstream.Parse(opts.DefaultScheme+"://"+line, opts.DefaultScheme, opts.DefaultTTL)
	if err != nil {
		return upstream.Upstream{}, err
	}
	up.Source = "provider"
	up.FetchedAt = opts.LocalNow
	up.ExpiresAt = opts.LocalNow.Add(opts.DefaultTTL)
	up.ID = fmt.Sprintf("%s@%d", up.BaseURL(), up.FetchedAt.Unix())
	return up, nil
}

func expiryFromProvider(expiredMS int64, opts ParseOptions) time.Time {
	if expiredMS <= 0 {
		return opts.LocalNow.Add(opts.DefaultTTL)
	}
	providerExpiry := time.UnixMilli(expiredMS)
	ttl := providerExpiry.Sub(opts.ServerNow)
	if ttl <= 0 || ttl > 24*time.Hour {
		return opts.LocalNow.Add(opts.DefaultTTL)
	}
	return opts.LocalNow.Add(ttl)
}
