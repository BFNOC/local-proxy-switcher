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
	prev, _ := s.Current()
	from := ""
	if prev.Scheme != "" {
		from = prev.RedactedURL()
	}
	copyUp := up
	s.current.Store(&copyUp)
	s.appendHistory(SwitchEvent{
		At:        time.Now(),
		From:      from,
		To:        copyUp.RedactedURL(),
		Reason:    reason,
		Interrupt: interrupt,
		Upstream:  &copyUp,
	})
	s.SetLastError("")
	if interrupt || (!interrupt && s.interruptDefault) {
		s.tracker.Interrupt()
	}
}

// Clear 清除当前上游锁定。
func (s *Selector) Clear(interrupt bool, reason string) {
	prev, _ := s.Current()
	from := ""
	if prev.Scheme != "" {
		from = prev.RedactedURL()
	}
	s.current.Store(nil)
	s.appendHistory(SwitchEvent{
		At:        time.Now(),
		From:      from,
		Reason:    reason,
		Interrupt: interrupt,
	})
	if interrupt {
		s.tracker.Interrupt()
	}
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

func (s *Selector) appendHistory(event SwitchEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, event)
	if len(s.history) > s.historyLimit {
		copy(s.history, s.history[len(s.history)-s.historyLimit:])
		s.history = s.history[:s.historyLimit]
	}
}
