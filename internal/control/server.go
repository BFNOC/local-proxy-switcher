package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/provider"
	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

// Options 配置本地控制 API。
type Options struct {
	Addr           string
	Selector       *selector.Selector
	Tracker        *tracker.Tracker
	Fetcher        *provider.Fetcher
	ManualTTL      time.Duration
	TCPingDial     func(context.Context, upstream.Target) (net.Conn, error)
	TCPingResolver tcpingResolver
}

// Server 暴露本地状态查询和切换操作。
type Server struct {
	addr           string
	selector       *selector.Selector
	tracker        *tracker.Tracker
	fetcher        *provider.Fetcher
	manualTTL      time.Duration
	tcpingDial     func(context.Context, upstream.Target) (net.Conn, error)
	tcpingResolver tcpingResolver
	server         *http.Server
}

type tcpingResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// StatusResponse 是控制 API 和 CLI 返回的 JSON 状态。
type StatusResponse struct {
	Current           *UpstreamStatus        `json:"current"`
	ActiveConnections int                    `json:"active_connections"`
	LastSwitch        string                 `json:"last_switch,omitempty"`
	LastError         string                 `json:"last_error,omitempty"`
	History           []selector.SwitchEvent `json:"history,omitempty"`
}

// UpstreamStatus 是当前上游的公开脱敏视图。
type UpstreamStatus struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Net       string `json:"net,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	ExpiresIn string `json:"expires_in,omitempty"`
	Source    string `json:"source,omitempty"`
}

// TCPingResponse 是当前上游到 HTTPS 目标的 TCP 连接探测结果。
type TCPingResponse struct {
	URL     string        `json:"url"`
	Target  string        `json:"target"`
	Via     string        `json:"via,omitempty"`
	Count   int           `json:"count"`
	Probes  []TCPingProbe `json:"probes"`
	Success bool          `json:"success"`
	AvgMS   int64         `json:"avg_ms,omitempty"`
	MinMS   int64         `json:"min_ms,omitempty"`
	MaxMS   int64         `json:"max_ms,omitempty"`
	Loss    int           `json:"loss"`
	Error   string        `json:"error,omitempty"`
}

// TCPingProbe 记录一次 TCP 连接探测。
type TCPingProbe struct {
	Seq       int    `json:"seq"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

const (
	defaultTCPingCount   = 3
	maxTCPingCount       = 5
	tcpingProbeTimeout   = 3 * time.Second
	tcpingResolveTimeout = 2 * time.Second
	maxTCPingURLLength   = 2048
)

// NewServer 创建本地控制 API 服务。
func NewServer(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:17990"
	}
	if opts.Tracker == nil {
		opts.Tracker = tracker.New()
	}
	if opts.ManualTTL <= 0 {
		opts.ManualTTL = 10 * time.Minute
	}
	if opts.TCPingResolver == nil {
		opts.TCPingResolver = net.DefaultResolver
	}
	s := &Server{
		addr:           opts.Addr,
		selector:       opts.Selector,
		tracker:        opts.Tracker,
		fetcher:        opts.Fetcher,
		manualTTL:      opts.ManualTTL,
		tcpingDial:     opts.TCPingDial,
		tcpingResolver: opts.TCPingResolver,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/switch", s.handleSwitch)
	mux.HandleFunc("/lock", s.handleLock)
	mux.HandleFunc("/clear", s.handleClear)
	mux.HandleFunc("/tcping", s.handleTCPing)
	mux.HandleFunc("/ui", s.handleUI)
	mux.HandleFunc("/", s.handleRoot)
	s.server = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// ListenAndServe 运行控制 API，直到上下文取消。
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()
	err := s.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅停止控制 API。
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, s.status())
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !allowMutation(w, r) {
		return
	}
	if s.fetcher == nil {
		s.selector.SetLastError("provider is not configured")
		writeError(w, http.StatusBadRequest, "provider is not configured")
		return
	}
	up, err := s.fetcher.Fetch(r.Context())
	if err != nil {
		s.selector.SetLastError(err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.selector.Switch(up, wantInterrupt(r), "provider switch")
	writeJSON(w, s.status())
}

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !allowMutation(w, r) {
		return
	}
	raw, err := readLockURL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	up, err := upstream.Parse(raw, "http", s.manualTTL)
	if err != nil {
		s.selector.SetLastError(err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	up.Source = "manual"
	s.selector.Switch(up, wantInterrupt(r), "manual lock")
	writeJSON(w, s.status())
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !allowMutation(w, r) {
		return
	}
	s.selector.Clear(wantInterrupt(r), "manual clear")
	s.selector.SetLastError("")
	writeJSON(w, s.status())
}

func (s *Server) handleTCPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !allowMutation(w, r) {
		return
	}
	req, err := readTCPingRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := s.parseTCPingTarget(r.Context(), req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.selector == nil {
		writeError(w, http.StatusServiceUnavailable, "selector is not configured")
		return
	}
	cur, ok := s.selector.Current()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "no current upstream")
		return
	}
	dial := s.tcpingDial
	if dial == nil {
		var err error
		dial, err = tcpingDialerFor(cur)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	writeJSON(w, s.runTCPing(r.Context(), target, cur.RedactedURL(), req.Count, dial))
}

func (s *Server) status() StatusResponse {
	history := s.selector.History()
	resp := StatusResponse{
		ActiveConnections: s.tracker.Count(),
		LastError:         s.selector.LastError(),
		History:           history,
	}
	if len(history) > 0 {
		resp.LastSwitch = history[len(history)-1].At.Format(time.RFC3339)
	}
	if cur, ok := s.selector.Current(); ok {
		resp.Current = &UpstreamStatus{
			ID:     cur.ID,
			URL:    cur.RedactedURL(),
			Net:    cur.Net,
			Source: cur.Source,
		}
		if !cur.ExpiresAt.IsZero() {
			resp.Current.ExpiresAt = cur.ExpiresAt.Format(time.RFC3339)
			left := time.Until(cur.ExpiresAt).Round(time.Second)
			if left < 0 {
				left = 0
			}
			resp.Current.ExpiresIn = left.String()
		}
	}
	return resp
}

func readLockURL(r *http.Request) (string, error) {
	defer r.Body.Close()
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return "", fmt.Errorf("Content-Type 必须是 application/json")
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.URL == "" {
		return "", fmt.Errorf("missing url")
	}
	return payload.URL, nil
}

type tcpingRequest struct {
	URL   string `json:"url"`
	Count int    `json:"count"`
}

func readTCPingRequest(r *http.Request) (tcpingRequest, error) {
	defer r.Body.Close()
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return tcpingRequest{}, fmt.Errorf("Content-Type 必须是 application/json")
	}
	var payload tcpingRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&payload); err != nil {
		return tcpingRequest{}, err
	}
	payload.URL = strings.TrimSpace(payload.URL)
	if payload.URL == "" {
		return tcpingRequest{}, fmt.Errorf("missing url")
	}
	if len(payload.URL) > maxTCPingURLLength {
		return tcpingRequest{}, fmt.Errorf("url is too long")
	}
	if payload.Count <= 0 {
		payload.Count = defaultTCPingCount
	}
	if payload.Count > maxTCPingCount {
		payload.Count = maxTCPingCount
	}
	return payload, nil
}

type tcpingTarget struct {
	URL           string
	DisplayTarget string
	Target        upstream.Target
}

func (s *Server) parseTCPingTarget(ctx context.Context, raw string) (tcpingTarget, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return tcpingTarget{}, err
	}
	if strings.ToLower(u.Scheme) != "https" {
		return tcpingTarget{}, fmt.Errorf("tcping only supports https URLs")
	}
	if u.User != nil {
		return tcpingTarget{}, fmt.Errorf("url credentials are not allowed")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return tcpingTarget{}, fmt.Errorf("missing host")
	}
	if net.ParseIP(host) != nil {
		return tcpingTarget{}, fmt.Errorf("tcping target must be a domain name")
	}
	if len(host) > 253 {
		return tcpingTarget{}, fmt.Errorf("host is too long")
	}
	if isBlockedTCPingHost(host) {
		return tcpingTarget{}, fmt.Errorf("unsafe tcping host")
	}
	port, err := tcpingPort(u.Port())
	if err != nil {
		return tcpingTarget{}, err
	}
	if err := s.validateTCPingDomain(ctx, host); err != nil {
		return tcpingTarget{}, err
	}
	target := upstream.Target{Host: host, Port: port}
	normalizedURL := (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(port))}).String()
	if port == 443 {
		normalizedURL = (&url.URL{Scheme: "https", Host: host}).String()
	}
	return tcpingTarget{
		URL:           normalizedURL,
		DisplayTarget: target.Address(),
		Target:        target,
	}, nil
}

func tcpingPort(raw string) (int, error) {
	if raw == "" {
		return 443, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("invalid tcping port %q", raw)
	}
	if port != 443 && port != 8443 {
		return 0, fmt.Errorf("tcping only supports HTTPS ports 443 and 8443")
	}
	return port, nil
}

func (s *Server) validateTCPingDomain(ctx context.Context, host string) error {
	resolver := s.tcpingResolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	resolveCtx, cancel := context.WithTimeout(ctx, tcpingResolveTimeout)
	defer cancel()
	addrs, err := resolver.LookupIPAddr(resolveCtx, host)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return fmt.Errorf("host did not resolve")
	}
	for _, addr := range addrs {
		ip := addr.IP
		if !isSafeTCPingIP(ip) {
			return fmt.Errorf("unsafe tcping host")
		}
	}
	return nil
}

func isBlockedTCPingHost(host string) bool {
	normalized := strings.TrimSuffix(strings.ToLower(host), ".")
	return normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") || strings.HasSuffix(normalized, ".local")
}

func isSafeTCPingIP(ip net.IP) bool {
	return ip != nil &&
		ip.IsGlobalUnicast() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsUnspecified() &&
		!ip.IsMulticast()
}

func (s *Server) runTCPing(ctx context.Context, target tcpingTarget, via string, count int, dial func(context.Context, upstream.Target) (net.Conn, error)) TCPingResponse {
	resp := TCPingResponse{
		URL:    target.URL,
		Target: target.DisplayTarget,
		Via:    via,
		Count:  count,
		Probes: make([]TCPingProbe, 0, count),
	}
	var total int64
	var successes int
	for i := 1; i <= count; i++ {
		if err := ctx.Err(); err != nil {
			resp.Loss += count - i + 1
			if resp.Error == "" {
				resp.Error = err.Error()
			}
			break
		}
		probe := TCPingProbe{Seq: i}
		probeCtx, cancel := context.WithTimeout(ctx, tcpingProbeTimeout)
		start := time.Now()
		conn, err := dial(probeCtx, target.Target)
		elapsed := time.Since(start).Round(time.Millisecond).Milliseconds()
		cancel()
		if err != nil {
			probe.Error = err.Error()
			resp.Loss++
			if resp.Error == "" {
				resp.Error = err.Error()
			}
			resp.Probes = append(resp.Probes, probe)
			continue
		}
		_ = conn.Close()
		if elapsed < 0 {
			elapsed = 0
		}
		probe.OK = true
		probe.LatencyMS = elapsed
		total += elapsed
		if successes == 0 || elapsed < resp.MinMS {
			resp.MinMS = elapsed
		}
		if elapsed > resp.MaxMS {
			resp.MaxMS = elapsed
		}
		successes++
		resp.Probes = append(resp.Probes, probe)
	}
	if successes > 0 {
		resp.AvgMS = total / int64(successes)
	}
	resp.Success = resp.Loss == 0
	return resp
}

func tcpingDialerFor(up upstream.Upstream) (func(context.Context, upstream.Target) (net.Conn, error), error) {
	if up.Scheme == "direct" {
		return secureDirectTCPingDial, nil
	}
	dialer, err := upstream.NewDialer(up, tcpingProbeTimeout)
	if err != nil {
		return nil, err
	}
	return dialer.DialContext, nil
}

func secureDirectTCPingDial(ctx context.Context, target upstream.Target) (net.Conn, error) {
	dialer := net.Dialer{
		Timeout: tcpingProbeTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if !isSafeTCPingIP(ip) {
				return fmt.Errorf("unsafe tcping host")
			}
			return nil
		},
	}
	return dialer.DialContext(ctx, "tcp", target.Address())
}

func wantInterrupt(r *http.Request) bool {
	v := strings.ToLower(r.URL.Query().Get("interrupt"))
	return v == "1" || v == "true" || v == "yes"
}

func allowMutation(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Host) == "" {
		writeError(w, http.StatusForbidden, "缺少 Host 头")
		return false
	}
	if !isLoopbackRequestHost(r.Host) {
		writeError(w, http.StatusForbidden, "拒绝非本地 Host 控制请求")
		return false
	}
	for _, value := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
			writeError(w, http.StatusForbidden, "拒绝跨来源控制请求")
			return false
		}
	}
	return true
}

func isLoopbackRequestHost(hostport string) bool {
	host := hostport
	if splitHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = splitHost
	}
	host = strings.Trim(strings.TrimSuffix(strings.ToLower(host), "."), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
