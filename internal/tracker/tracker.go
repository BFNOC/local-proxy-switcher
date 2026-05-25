package tracker

import (
	"sync"
	"sync/atomic"
)

// Closeable 是连接追踪器用于中断资源的最小接口。
type Closeable interface {
	Close() error
}

// Tracker 记录活跃连接，便于手动中断时统一关闭。
type Tracker struct {
	next  atomic.Uint64
	mu    sync.Mutex
	conns map[uint64]Closeable
}

// New 创建空连接追踪器。
func New() *Tracker {
	return &Tracker{conns: make(map[uint64]Closeable)}
}

// Register 追踪一个可关闭资源，直到调用返回的注销函数。
func (t *Tracker) Register(conn Closeable) func() {
	if t == nil || conn == nil {
		return func() {}
	}
	id := t.next.Add(1)
	t.mu.Lock()
	t.conns[id] = conn
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.conns, id)
			t.mu.Unlock()
		})
	}
}

// Count 返回当前追踪的资源数量。
func (t *Tracker) Count() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// Interrupt 关闭所有当前追踪的资源，并返回尝试关闭的数量。
func (t *Tracker) Interrupt() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	conns := make([]Closeable, 0, len(t.conns))
	for _, conn := range t.conns {
		conns = append(conns, conn)
	}
	t.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return len(conns)
}
