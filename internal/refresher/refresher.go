package refresher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BFNOC/local-proxy-switcher/internal/selector"
	"github.com/BFNOC/local-proxy-switcher/internal/upstream"
)

const (
	defaultMinRefreshLead = 5 * time.Second
	defaultMaxRefreshLead = 30 * time.Second
	defaultMinRetryDelay  = 5 * time.Second
	defaultMaxRetryDelay  = time.Minute
)

var errStaleRefresh = errors.New("auto refresh result discarded because selector changed")

// Fetcher 获取新的 provider 上游。
type Fetcher interface {
	Fetch(context.Context) (upstream.Upstream, error)
}

// Options 配置自动刷新循环。
type Options struct {
	Fetcher        Fetcher
	Selector       *selector.Selector
	Interrupt      bool
	MinRefreshLead time.Duration
	MaxRefreshLead time.Duration
	MinRetryDelay  time.Duration
	MaxRetryDelay  time.Duration
	Now            func() time.Time
}

// Refresher 在 provider 上游接近过期时自动拉取并切换。
type Refresher struct {
	fetcher        Fetcher
	selector       *selector.Selector
	interrupt      bool
	minRefreshLead time.Duration
	maxRefreshLead time.Duration
	minRetryDelay  time.Duration
	maxRetryDelay  time.Duration
	now            func() time.Time
}

// New 使用安全默认值创建自动刷新器。
func New(opts Options) *Refresher {
	if opts.MinRefreshLead <= 0 {
		opts.MinRefreshLead = defaultMinRefreshLead
	}
	if opts.MaxRefreshLead <= 0 {
		opts.MaxRefreshLead = defaultMaxRefreshLead
	}
	if opts.MinRetryDelay <= 0 {
		opts.MinRetryDelay = defaultMinRetryDelay
	}
	if opts.MaxRetryDelay <= 0 {
		opts.MaxRetryDelay = defaultMaxRetryDelay
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Refresher{
		fetcher:        opts.Fetcher,
		selector:       opts.Selector,
		interrupt:      opts.Interrupt,
		minRefreshLead: opts.MinRefreshLead,
		maxRefreshLead: opts.MaxRefreshLead,
		minRetryDelay:  opts.MinRetryDelay,
		maxRetryDelay:  opts.MaxRetryDelay,
		now:            opts.Now,
	}
}

// Run 运行自动刷新循环，直到 ctx 取消。
func (r *Refresher) Run(ctx context.Context) {
	if r == nil || r.fetcher == nil || r.selector == nil {
		return
	}
	changes := r.selector.Watch()
	defer r.selector.Unwatch(changes)
	lastFailed := false
	retryCount := 0

	for {
		delay, ok, version := r.nextDelay(r.now(), lastFailed, retryCount)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-changes:
				lastFailed = false
				retryCount = 0
				continue
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return
		case <-changes:
			stopTimer(timer)
			lastFailed = false
			retryCount = 0
			continue
		case <-timer.C:
		}

		// version 来自调度快照；网络拉取返回后 selector 会用它丢弃过期结果。
		if err := r.refresh(ctx, version); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.selector.SetProviderRefreshError(fmt.Sprintf("auto refresh failed: %v", err), version)
			lastFailed = true
			retryCount++
			continue
		}
		lastFailed = false
		retryCount = 0
	}
}

func (r *Refresher) refresh(ctx context.Context, version uint64) error {
	up, err := r.fetcher.Fetch(ctx)
	if err != nil {
		return err
	}
	if err := r.validateFetched(up); err != nil {
		return err
	}
	up.Source = "provider"
	// Selector 会在同一把锁下完成最终校验和切换，避免迟到的 provider
	// 拉取结果覆盖用户刚执行的手动 lock 或 clear。
	if !r.selector.SwitchProviderRefresh(up, r.interrupt, "auto refresh", version) {
		return errStaleRefresh
	}
	return nil
}

// validateFetched 把 provider 返回的已过期或几乎过期上游视为失败，避免切换后高频刷新。
func (r *Refresher) validateFetched(up upstream.Upstream) error {
	if up.ExpiresAt.IsZero() {
		return nil
	}
	remaining := up.ExpiresAt.Sub(r.now())
	if remaining <= 0 {
		return fmt.Errorf("provider returned expired upstream: %s expired at %s", up.RedactedURL(), up.ExpiresAt.Format(time.RFC3339))
	}
	if remaining < r.minRefreshLead {
		return fmt.Errorf("provider returned near-expired upstream: %s expires in %s", up.RedactedURL(), remaining)
	}
	return nil
}

// nextDelay 返回下一次刷新等待时间；ok=false 表示暂停直到 selector 变化。
func (r *Refresher) nextDelay(now time.Time, lastFailed bool, retryCount int) (time.Duration, bool, uint64) {
	cur, ok, paused, version := r.selector.ProviderRefreshSnapshot()
	if lastFailed {
		return r.backoffDelay(retryCount), true, version
	}

	if !ok {
		if paused {
			return 0, false, version
		}
		return 0, true, version
	}
	if cur.Source != "provider" || cur.ExpiresAt.IsZero() {
		return 0, false, version
	}

	refreshAt := cur.ExpiresAt.Add(-r.refreshLead(cur, now))
	if !refreshAt.After(now) {
		return 0, true, version
	}
	return refreshAt.Sub(now), true, version
}

// refreshLead 使用 TTL 的 10% 作为提前量，并限制在安全边界内。
func (r *Refresher) refreshLead(up upstream.Upstream, now time.Time) time.Duration {
	base := up.FetchedAt
	if base.IsZero() || !base.Before(up.ExpiresAt) {
		base = now
	}
	ttl := up.ExpiresAt.Sub(base)
	if ttl <= 0 {
		return 0
	}

	lead := ttl / 10
	if lead < r.minRefreshLead {
		lead = r.minRefreshLead
	}
	if lead > r.maxRefreshLead {
		lead = r.maxRefreshLead
	}
	if half := ttl / 2; half > 0 && lead > half {
		lead = half
	}
	return lead
}

// backoffDelay 对失败刷新做指数退避，避免 provider 抖动时频繁请求。
func (r *Refresher) backoffDelay(retryCount int) time.Duration {
	if retryCount <= 1 {
		return r.minRetryDelay
	}
	delay := r.minRetryDelay
	for i := 1; i < retryCount; i++ {
		if delay >= r.maxRetryDelay/2 {
			return r.maxRetryDelay
		}
		delay *= 2
	}
	if delay > r.maxRetryDelay {
		return r.maxRetryDelay
	}
	return delay
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
