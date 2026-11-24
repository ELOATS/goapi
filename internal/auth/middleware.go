package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"ELOATS/goapi/internal/httpx"
)

const realm = "goapi"

// Authenticator 是认证中间件。
type Authenticator struct {
	verifier Verifier
	dev      *Principal
	logger   *slog.Logger
}

func New(v Verifier, logger *slog.Logger) *Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{verifier: v, logger: logger}
}

// Disabled 不校验任何 token，所有请求都当作 p。只用于本地开发和测试；
// config 包禁止在 APP_ENV=prod 时启用。
func Disabled(p Principal, logger *slog.Logger) *Authenticator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{dev: &p, logger: logger}
}

// Middleware 要求请求带有效的 Bearer token（RFC 6750），否则返回 401。
// 只接受 Authorization 头，不接受查询参数里的 token（会进访问日志和代理日志）。
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.dev != nil {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), *a.dev)))
			return
		}

		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			httpx.Error(w, r, http.StatusUnauthorized, "missing bearer token")
			return
		}

		p, err := a.verifier.Verify(r.Context(), raw)
		if errors.Is(err, ErrKeysUnavailable) {
			// 不是 token 的问题：返回 503，客户端稍后重试即可，不要清掉 token 或让用户重新登录
			a.logger.WarnContext(r.Context(), "cannot verify token: signing keys unavailable", "err", err)
			w.Header().Set("Retry-After", "5")
			httpx.Error(w, r, http.StatusServiceUnavailable, "authentication temporarily unavailable")
			return
		}
		if err != nil {
			// 过期 token 是常态，用 Debug 级别；错误细节只进日志，不返回给客户端
			a.logger.DebugContext(r.Context(), "token rejected", "err", err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`", error="invalid_token"`)
			httpx.Error(w, r, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireAnyRole 要求调用方至少拥有其中一个角色：未认证 401，没有权限 403。
// 必须放在 Authenticator.Middleware 之后。
func RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
				httpx.Error(w, r, http.StatusUnauthorized, "authentication required")
				return
			}
			if !p.HasAnyRole(roles...) {
				httpx.Error(w, r, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
