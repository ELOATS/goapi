package httpserver

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	apispec "ELOATS/goapi/api"
	"ELOATS/goapi/internal/auth"
	"ELOATS/goapi/internal/config"
	"ELOATS/goapi/internal/handler"
	"ELOATS/goapi/internal/httpx"
	"ELOATS/goapi/internal/ratelimit"
)

// 角色名需要在身份提供方里创建，并分配给用户或客户端。
const (
	RoleViewer = "goapi-viewer" // 只读
	RoleAdmin  = "goapi-admin"  // 读写
)

func NewRouter(cfg config.Config, logger *slog.Logger, deps Deps, isReady func() bool) http.Handler {
	if deps.Auth == nil {
		panic("httpserver: Deps.Auth is required (use auth.Disabled for local development)")
	}
	r := chi.NewRouter()

	// 顺序很重要：RequestID → RealIP → 日志 → panic 恢复
	r.Use(middleware.RequestID)
	if cfg.TrustProxy {
		r.Use(realIP(cfg.ClientIPHeader))
	}
	r.Use(observe(deps.Metrics)) // 在 recoverer 外层：panic 也会被计为 500
	r.Use(requestLogger(logger))
	r.Use(recoverer(logger))
	// 响应压缩：客户端带 Accept-Encoding 时压缩 JSON / YAML / HTML（列表接口通常能小 70% 以上）。
	// 响应里没有 cookie，认证走 Authorization 头，BREACH 这类基于压缩的攻击无从下手
	r.Use(middleware.Compress(5, "application/json", "application/yaml", "text/html"))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusMethodNotAllowed, "method not allowed")
	})

	// 存活探针：进程活着就返回 200
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// 就绪探针：关闭期间返回 503，让 LB / k8s 摘流量
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !isReady() {
			httpx.Error(w, r, http.StatusServiceUnavailable, "shutting down")
			return
		}
		if deps.HealthCheck != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := deps.HealthCheck(ctx); err != nil {
				logger.WarnContext(ctx, "readiness check failed", "err", err)
				httpx.Error(w, r, http.StatusServiceUnavailable, "dependency unavailable")
				return
			}
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// API 文档：不需要认证。网关暴露了 /api 前缀，不想公开时设 DOCS_ENABLED=false
	if cfg.DocsEnabled {
		r.Get("/api/openapi.yaml", apispec.SpecHandler)
		r.Get("/api/docs", apispec.UIHandler)
	}

	cursorKey := cfg.CursorKey
	if len(cursorKey) == 0 {
		// 本地开发和测试：每次启动随机生成（cmd/api 会打印警告）；生产环境由配置校验保证不为空
		cursorKey = make([]byte, 32)
		_, _ = rand.Read(cursorKey)
	}
	cursors, err := handler.NewCursorCodec(cursorKey, cfg.CursorKeysPrevious...)
	if err != nil {
		panic("httpserver: " + err.Error()) // 配置校验已经检查过密钥长度，走到这里是程序错误
	}
	users := handler.NewUserHandler(deps.Users, cfg.MaxBodyBytes, cursors)

	// 探针在上面，不需要认证；/api/v1 下全部需要有效 token
	r.Route("/api/v1", func(r chi.Router) {
		// 1) 按 IP 限流放在认证之前：带无效 token 的洪水在验签之前就被挡住
		if deps.RateLimitIP != nil {
			r.Use(ratelimit.Middleware(deps.RateLimitIP, ratelimit.Options{
				Key: ratelimit.ByClientIP, Metrics: deps.RateLimitMetrics, Logger: logger,
			}))
		}
		// 2) 认证
		r.Use(deps.Auth.Middleware)
		// 3) 按用户限流：同一个账号换 IP 也绕不过去；多人共用一个出口 IP 也不会互相影响太多
		if deps.RateLimitUser != nil {
			r.Use(ratelimit.Middleware(deps.RateLimitUser, ratelimit.Options{
				Key: bySubject, Metrics: deps.RateLimitMetrics, Logger: logger,
			}))
		}
		r.Use(middleware.Timeout(cfg.HandlerTimeout))
		r.Use(middleware.AllowContentType("application/json"))

		r.Get("/me", handler.Me)

		read := auth.RequireAnyRole(RoleViewer, RoleAdmin)
		write := auth.RequireAnyRole(RoleAdmin)
		// 4) 写接口单独限流，放在角色检查之后：没有权限的请求直接 403，不占写额度
		writeLimit := func(next http.Handler) http.Handler { return next }
		if deps.RateLimitWrite != nil {
			writeLimit = ratelimit.Middleware(deps.RateLimitWrite, ratelimit.Options{
				Key: bySubject, Metrics: deps.RateLimitMetrics, Logger: logger,
			})
		}
		r.Route("/users", func(r chi.Router) {
			r.With(read).Get("/", users.List)
			r.With(write, writeLimit).Post("/", users.Create)
			r.With(read).Get("/{id}", users.Get)
		})
	})

	return r
}

func bySubject(r *http.Request) (string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" {
		return "", false
	}
	return p.Subject, true
}
