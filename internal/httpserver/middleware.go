package httpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"ELOATS/goapi/internal/httpx"
	"ELOATS/goapi/internal/telemetry"
)

// observe 记录 RED 指标，并把 span 重命名为 "GET /api/v1/users/{id}"。
// 路由模板只有在 chi 完成匹配后才知道，所以在 next.ServeHTTP 之后读取。
func observe(m *telemetry.HTTPMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			m.Begin()

			next.ServeHTTP(ww, r)

			route := "unmatched" // 404 统一归到一个标签值，防止扫描器制造高基数
			if rc := chi.RouteContext(r.Context()); rc != nil {
				if p := rc.RoutePattern(); p != "" && p != "/*" {
					route = p
				}
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}

			span := trace.SpanFromContext(r.Context())
			span.SetName(r.Method + " " + route)
			span.SetAttributes(attribute.String("http.route", route))

			m.End(r.Method, route, status, time.Since(start))
		})
	}
}

// requestLogger 用 slog 输出结构化访问日志。
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case status >= 400:
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_ip", r.RemoteAddr),
			)
		})
	}
}

// recoverer 捕获 panic，记录堆栈并返回 JSON 500，而不是让连接直接断开。
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec) // 交给 net/http 处理，保持语义
					}
					logger.ErrorContext(r.Context(), "panic recovered",
						"request_id", middleware.GetReqID(r.Context()),
						"panic", rec,
						"stack", string(debug.Stack()),
					)
					httpx.Error(w, r, http.StatusInternalServerError, "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// realIP 用可信代理设置的单值头（如 X-Envoy-External-Address、X-Real-IP）覆盖 RemoteAddr，后面的日志和按 IP 限流都用它。
// 不用 chi 自带的 middleware.RealIP：它会按顺序信任 True-Client-IP、X-Real-IP、X-Forwarded-For，
// 而很多代理会原样转发客户端自带的 True-Client-IP 等头，客户端就能随意伪造 IP 绕过限流。
// 头的值不是合法 IP（例如多值的 X-Forwarded-For）时保持原样。
func realIP(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
				if addr, err := netip.ParseAddr(v); err == nil {
					r.RemoteAddr = addr.String()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
