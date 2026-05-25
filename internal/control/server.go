package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/provider"
	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

// Options 配置本地控制 API。
type Options struct {
	Addr      string
	Selector  *selector.Selector
	Tracker   *tracker.Tracker
	Fetcher   *provider.Fetcher
	ManualTTL time.Duration
}

// Server 暴露本地状态查询和切换操作。
type Server struct {
	addr      string
	selector  *selector.Selector
	tracker   *tracker.Tracker
	fetcher   *provider.Fetcher
	manualTTL time.Duration
	server    *http.Server
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
	s := &Server{
		addr:      opts.Addr,
		selector:  opts.Selector,
		tracker:   opts.Tracker,
		fetcher:   opts.Fetcher,
		manualTTL: opts.ManualTTL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/switch", s.handleSwitch)
	mux.HandleFunc("/lock", s.handleLock)
	mux.HandleFunc("/clear", s.handleClear)
	mux.HandleFunc("/ui", s.handleUI)
	mux.HandleFunc("/", s.handleRoot)
	s.server = &http.Server{Addr: opts.Addr, Handler: mux}
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

func wantInterrupt(r *http.Request) bool {
	v := strings.ToLower(r.URL.Query().Get("interrupt"))
	return v == "1" || v == "true" || v == "yes"
}

func allowMutation(w http.ResponseWriter, r *http.Request) bool {
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
