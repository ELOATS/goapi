// Package httpserver 负责 HTTP 服务器、路由与中间件的组装。
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"example.com/goapi/internal/auth"
	"example.com/goapi/internal/config"
	"example.com/goapi/internal/handler"
	"example.com/goapi/internal/ratelimit"
	"example.com/goapi/internal/telemetry"
)

// Deps 是注入到路由层的依赖，方便测试时替换。
type Deps struct {
	Users handler.UserService
	// HealthCheck 供 /readyz 检查下游依赖（如数据库）；为 nil 时跳过。
	HealthCheck func(context.Context) error
	// Metrics 为 nil 时不记录 HTTP 指标。
	Metrics *telemetry.HTTPMetrics
	// Auth 必填。本地开发用 auth.Disabled。
	Auth *auth.Authenticator
	// 限流：为 nil 时不启用对应规则
	RateLimitIP      ratelimit.Limiter
	RateLimitUser    ratelimit.Limiter
	RateLimitWrite   ratelimit.Limiter // 只用于写接口，按用户计数
	RateLimitMetrics *ratelimit.Metrics
}

type Server struct {
	srv   *http.Server
	ready atomic.Bool
}

func New(cfg config.Config, logger *slog.Logger, deps Deps) *Server {
	s := &Server{}
	s.ready.Store(true)
	s.srv = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           withTracing(NewRouter(cfg, logger, deps, s.ready.Load)),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout, // 防 slowloris
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

func (s *Server) ListenAndServe() error             { return s.srv.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }
func (s *Server) SetNotReady()                       { s.ready.Store(false) }

// withTracing 放在最外层：解析上游 traceparent、创建 server span，
// 之后所有中间件和 handler 的 ctx 里都带着 span。探针请求不产生 span，避免噪音。
func withTracing(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
	)
}
