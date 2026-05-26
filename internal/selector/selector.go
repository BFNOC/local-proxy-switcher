package selector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/tracker"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

// Options 配置选择器的切换、过期和中断行为。
type Options struct {
	Tracker             *tracker.Tracker
	DialTimeout         time.Duration
	FailClosedOnExpired bool
	FallbackToDirect    bool
	HistoryLimit        int
	InterruptByDefault  bool
}

// Selector 保存当前锁定的上游，并把它应用到新连接。
type Selector struct {
	current            atomic.Pointer[upstream.Upstream]
	tracker            *tracker.Tracker
	dialTimeout        time.Duration
	failClosedOnExpiry bool
	fallbackToDirect   bool
	interruptDefault   bool

	mu           sync.Mutex
	history      []SwitchEvent
	historyLimit int
	lastError    string
	watchers     []chan struct{}

	providerRefreshPaused bool
	version               uint64
}

// SwitchEvent 记录一次锁定、切换或清空操作。
type SwitchEvent struct {
	At        time.Time          `json:"at"`
	From      string             `json:"from,omitempty"`
	To        string             `json:"to,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Interrupt bool               `json:"interrupt"`
	Upstream  *upstream.Upstream `json:"upstream,omitempty"`
}

// New 使用安全默认值创建选择器。
func New(opts Options) *Selector {
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 3 * time.Second
	}
	if opts.HistoryLimit <= 0 {
		opts.HistoryLimit = 50
	}
	if opts.Tracker == nil {
		opts.Tracker = tracker.New()
	}
	return &Selector{
		tracker:            opts.Tracker,
		dialTimeout:        opts.DialTimeout,
		failClosedOnExpiry: opts.FailClosedOnExpired,
		fallbackToDirect:   opts.FallbackToDirect,
		interruptDefault:   opts.InterruptByDefault,
		historyLimit:       opts.HistoryLimit,
	}
}

// Current 返回当前锁定的上游。
func (s *Selector) Current() (upstream.Upstream, bool) {
	ptr := s.current.Load()
	if ptr == nil {
		return upstream.Upstream{}, false
	}
	return *ptr, true
}

// Switch 原子锁定新上游，后续新连接会使用它。
func (s *Selector) Switch(up upstream.Upstream, interrupt bool, reason string) {
	copyUp := up
	s.mu.Lock()
	watchers := s.switchLocked(copyUp, interrupt, reason)
	s.mu.Unlock()

	if interrupt || (!interrupt && s.interruptDefault) {
		s.tracker.Interrupt()
	}
	notifyWatchers(watchers)
}

// Clear 清除当前上游锁定。
func (s *Selector) Clear(interrupt bool, reason string) {
	s.mu.Lock()
	prev := s.current.Load()
	from := ""
	if prev != nil && prev.Scheme != "" {
		from = prev.RedactedURL()
	}
	s.current.Store(nil)
	s.appendHistoryLocked(SwitchEvent{
		At:        time.Now(),
		From:      from,
		Reason:    reason,
		Interrupt: interrupt,
	})
	s.providerRefreshPaused = true
	s.version++
	watchers := s.watchersLocked()
	s.mu.Unlock()

	if interrupt {
		s.tracker.Interrupt()
	}
	notifyWatchers(watchers)
}

// Dial 使用当前锁定上游打开到目标的连接。
func (s *Selector) Dial(ctx context.Context, target upstream.Target) (net.Conn, error) {
	up, ok := s.Current()
	if !ok {
		if s.fallbackToDirect {
			return upstream.DirectDialer{Timeout: s.dialTimeout}.DialContext(ctx, target)
		}
		return nil, errors.New("no current upstream")
	}
	if up.IsExpired(time.Now()) && s.failClosedOnExpiry {
		if s.fallbackToDirect {
			return upstream.DirectDialer{Timeout: s.dialTimeout}.DialContext(ctx, target)
		}
		return nil, fmt.Errorf("current upstream expired: %s", up.RedactedURL())
	}
	dialer, err := upstream.NewDialer(up, s.dialTimeout)
	if err != nil {
		return nil, err
	}
	return dialer.DialContext(ctx, target)
}

// History 返回最近切换事件的副本。
func (s *Selector) History() []SwitchEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SwitchEvent, len(s.history))
	copy(out, s.history)
	return out
}

// LastError 返回最近一次控制面错误。
func (s *Selector) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

// SetLastError 保存最近一次控制面错误。
func (s *Selector) SetLastError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = msg
}

// ProviderRefreshSnapshot 返回自动刷新调度需要的同一时刻状态快照。
func (s *Selector) ProviderRefreshSnapshot() (upstream.Upstream, bool, bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ptr := s.current.Load()
	if ptr == nil {
		return upstream.Upstream{}, false, s.providerRefreshPaused, s.version
	}
	return *ptr, true, s.providerRefreshPaused, s.version
}

// Watch 返回一个在当前上游变化时收到通知的通道。
func (s *Selector) Watch() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchers = append(s.watchers, ch)
	return ch
}

// Unwatch 注销 Watch 返回的通道，避免后台任务重启时残留订阅者。
func (s *Selector) Unwatch(ch <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, watcher := range s.watchers {
		if watcher == ch {
			copy(s.watchers[i:], s.watchers[i+1:])
			s.watchers[len(s.watchers)-1] = nil
			s.watchers = s.watchers[:len(s.watchers)-1]
			return
		}
	}
}

// SetProviderRefreshError 只在状态未变化时记录后台刷新错误，避免覆盖新的手动切换结果。
func (s *Selector) SetProviderRefreshError(msg string, expectedVersion uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != expectedVersion || !s.providerRefreshAllowedLocked() {
		return false
	}
	s.lastError = msg
	return true
}

// SwitchProviderRefresh 只在当前状态仍属于 provider 自动化时应用后台刷新结果。
func (s *Selector) SwitchProviderRefresh(up upstream.Upstream, interrupt bool, reason string, expectedVersion uint64) bool {
	copyUp := up
	s.mu.Lock()
	if s.version != expectedVersion || !s.providerRefreshAllowedLocked() {
		s.mu.Unlock()
		return false
	}
	watchers := s.switchLocked(copyUp, interrupt, reason)
	s.mu.Unlock()

	if interrupt || (!interrupt && s.interruptDefault) {
		s.tracker.Interrupt()
	}
	notifyWatchers(watchers)
	return true
}

func (s *Selector) switchLocked(up upstream.Upstream, interrupt bool, reason string) []chan struct{} {
	from := ""
	if prev := s.current.Load(); prev != nil && prev.Scheme != "" {
		from = prev.RedactedURL()
	}
	s.current.Store(&up)
	s.appendHistoryLocked(SwitchEvent{
		At:        time.Now(),
		From:      from,
		To:        up.RedactedURL(),
		Reason:    reason,
		Interrupt: interrupt,
		Upstream:  &up,
	})
	s.lastError = ""
	s.providerRefreshPaused = false
	s.version++
	return s.watchersLocked()
}

func (s *Selector) appendHistoryLocked(event SwitchEvent) {
	s.history = append(s.history, event)
	if len(s.history) > s.historyLimit {
		copy(s.history, s.history[len(s.history)-s.historyLimit:])
		s.history = s.history[:s.historyLimit]
	}
}

func (s *Selector) providerRefreshAllowedLocked() bool {
	cur := s.current.Load()
	if cur == nil {
		return !s.providerRefreshPaused
	}
	return cur.Source == "provider"
}

func (s *Selector) watchersLocked() []chan struct{} {
	watchers := make([]chan struct{}, len(s.watchers))
	copy(watchers, s.watchers)
	return watchers
}

// notifyWatchers 非阻塞唤醒订阅者，避免 selector 写路径被后台任务卡住。
func notifyWatchers(watchers []chan struct{}) {
	for _, ch := range watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
