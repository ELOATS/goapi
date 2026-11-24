package ratelimit

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"ELOATS/goapi/internal/httpx"
)

// KeyFunc 返回限流的键；ok 为 false 表示这个请求不受该规则约束。
type KeyFunc func(r *http.Request) (key string, ok bool)

type Options struct {
	Key     KeyFunc       // 必填
	Metrics *Metrics      // 可为 nil
	Logger  *slog.Logger  // 可为 nil
	Timeout time.Duration // 单次判定的超时，默认 200ms
}

const errorLogEvery = 10 * time.Second

// Middleware 超出限额时返回 429 和 Retry-After，并在每个响应上带 RateLimit-Limit / -Remaining / -Reset。
// 多个限流中间件叠加时，响应头以最后一个（最内层）为准。
//
// 限流器出错（例如 Redis 不可用）时放行请求（fail open）：限流是保护手段，
// 它自己出故障不应该让整个 API 不可用。出错会计入指标并触发告警。
func Middleware(l Limiter, opt Options) func(http.Handler) http.Handler {
	if opt.Key == nil {
		panic("ratelimit: Options.Key is required")
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 200 * time.Millisecond
	}
	policy := l.Policy().Name
	var lastErrLog atomic.Int64 // Redis 挂掉时每个请求都会出错，日志限频

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := opt.Key(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), opt.Timeout)
			res, err := l.Allow(ctx, key)
			cancel()
			if err != nil {
				opt.Metrics.observe(policy, "error")
				if now := time.Now().UnixNano(); now-lastErrLog.Load() > int64(errorLogEvery) {
					lastErrLog.Store(now)
					opt.Logger.WarnContext(r.Context(), "rate limiter failed, allowing request",
						"policy", policy, "err", err)
				}
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("RateLimit-Limit", strconv.Itoa(res.Limit))
			h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
			h.Set("RateLimit-Reset", strconv.Itoa(ceilSeconds(res.ResetAfter)))
			if !res.Allowed {
				opt.Metrics.observe(policy, "limited")
				h.Set("Retry-After", strconv.Itoa(max(1, ceilSeconds(res.RetryAfter))))
				httpx.Error(w, r, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			opt.Metrics.observe(policy, "allowed")
			next.ServeHTTP(w, r)
		})
	}
}

// ByClientIP 按客户端 IP 限流。IPv6 按 /64 归并：一个用户通常能拿到整个 /64，
// 按单个地址计数的话换个地址就能绕过。
// 部署在反向代理后面时，需要先用可信的头把 RemoteAddr 改成真实客户端 IP（见 httpserver.realIP）。
func ByClientIP(r *http.Request) (string, bool) {
	return clientIP(r.RemoteAddr), true
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // 已经是不带端口的 IP
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// Metrics 记录限流判定结果。所有方法对 nil 接收者安全。
type Metrics struct {
	decisions *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	return &Metrics{decisions: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "http_rate_limit_decisions_total",
		Help: "Rate limit decisions by policy and result (allowed, limited, error).",
	}, []string{"policy", "result"})}
}

func (m *Metrics) observe(policy, result string) {
	if m != nil {
		m.decisions.WithLabelValues(policy, result).Inc()
	}
}
