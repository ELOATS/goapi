// Package ratelimit 用 GCRA（Generic Cell Rate Algorithm，等价于令牌桶）做请求限流。
// 内存实现用于单实例和本地开发；Redis 实现让多个副本共享同一份计数。
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Policy 描述一条限流规则：长期平均 Rate 个/秒，最多允许连续 Burst 个突发请求。
type Policy struct {
	Name  string  // 用于 Redis 键前缀和指标标签，例如 "ip"、"user"
	Rate  float64 // 每秒补充的请求数
	Burst int     // 桶容量
}

func (p Policy) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("ratelimit: policy name is required")
	}
	if !(p.Rate > 0 && p.Rate <= 1e6) {
		return fmt.Errorf("ratelimit: policy %q rate must be in (0, 1e6], got %v", p.Name, p.Rate)
	}
	if p.Burst < 1 || p.Burst > 1e6 {
		return fmt.Errorf("ratelimit: policy %q burst must be in [1, 1e6], got %d", p.Name, p.Burst)
	}
	return nil
}

// interval 是补充一个请求所需的时间。
func (p Policy) interval() time.Duration { return time.Duration(float64(time.Second) / p.Rate) }

// tolerance 是允许"透支"的总时长，等于 Burst 个 interval。
func (p Policy) tolerance() time.Duration { return p.interval() * time.Duration(p.Burst) }

type Result struct {
	Allowed    bool
	Limit      int           // 桶容量（Burst）
	Remaining  int           // 本次之后还能立即发出的请求数
	RetryAfter time.Duration // 被拒绝时，多久之后可以重试
	ResetAfter time.Duration // 多久之后额度完全恢复
}

type Limiter interface {
	Allow(ctx context.Context, key string) (Result, error)
	Policy() Policy
}

// gcra 给定当前时间和该键的 TAT（theoretical arrival time），返回判定结果和新的 TAT。
// 被拒绝时 TAT 不变：被拒绝的请求不消耗额度。
func gcra(p Policy, now, tat time.Time) (time.Time, Result) {
	interval, tolerance := p.interval(), p.tolerance()
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(interval)
	diff := next.Sub(now)

	r := Result{Limit: p.Burst}
	if diff > tolerance {
		r.RetryAfter = diff - tolerance
		r.ResetAfter = tat.Sub(now)
		return tat, r
	}
	r.Allowed = true
	r.Remaining = int((tolerance - diff) / interval)
	r.ResetAfter = diff
	return next, r
}

// Memory 是单进程内的限流器。多副本部署时每个副本单独计数，
// 实际总限额约等于 限额 × 副本数；需要精确的全局限额请用 Redis。
type Memory struct {
	policy Policy
	now    func() time.Time

	mu        sync.Mutex
	tats      map[string]time.Time
	lastSweep time.Time
}

// sweepEvery 控制多久清理一次已经完全恢复的键，防止 map 无限增长（例如扫描器换着 IP 打）。
const sweepEvery = time.Minute

func NewMemory(p Policy) (*Memory, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Memory{policy: p, now: time.Now, tats: map[string]time.Time{}, lastSweep: time.Now()}, nil
}

func (m *Memory) Policy() Policy { return m.policy }

func (m *Memory) Allow(_ context.Context, key string) (Result, error) {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if now.Sub(m.lastSweep) >= sweepEvery {
		// TAT 不晚于现在的键，额度已完全恢复，和"从没出现过"等价，可以删掉
		for k, t := range m.tats {
			if !t.After(now) {
				delete(m.tats, k)
			}
		}
		m.lastSweep = now
	}

	tat, r := gcra(m.policy, now, m.tats[key])
	if r.Allowed {
		m.tats[key] = tat
	}
	return r, nil
}

// Len 返回当前跟踪的键数量，供测试和调试使用。
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tats)
}
