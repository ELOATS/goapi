package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ELOATS/goapi/internal/auth"
	"ELOATS/goapi/internal/auth/authtest"
	"ELOATS/goapi/internal/domain"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// echoSubject 把调用方的 sub 写回响应，用来确认中间件放行并注入了 Principal。
var echoSubject = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	io.WriteString(w, p.Subject)
})

func TestMiddleware(t *testing.T) {
	iss := authtest.New(t)
	other := authtest.New(t) // 同样的 issuer / audience，不同的私钥
	h := auth.New(iss.Verifier("roles"), discard).Middleware(echoSubject)
	past := time.Now().Add(-time.Hour).Unix()

	cases := []struct {
		name        string
		header      string
		want        int
		wantInvalid bool // WWW-Authenticate 是否带 error="invalid_token"
	}{
		{"missing header", "", 401, false},
		{"basic scheme", "Basic dXNlcjpwYXNz", 401, false},
		{"empty bearer", "Bearer ", 401, false},
		{"valid", "Bearer " + iss.Token(t, nil), 200, false},
		{"scheme is case-insensitive", "bearer " + iss.Token(t, nil), 200, false},
		{"expired", "Bearer " + iss.Token(t, map[string]any{"exp": past}), 401, true},
		{"no exp", "Bearer " + iss.Token(t, map[string]any{"exp": nil}), 401, true},
		{"wrong audience", "Bearer " + iss.Token(t, map[string]any{"aud": "other-app"}), 401, true},
		{"wrong issuer", "Bearer " + iss.Token(t, map[string]any{"iss": "https://evil.test"}), 401, true},
		{"no sub", "Bearer " + iss.Token(t, map[string]any{"sub": nil}), 401, true},
		{"signed by another key", "Bearer " + other.Token(t, nil), 401, true},
		{"not a jwt", "Bearer not-a-jwt", 401, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want == 200 {
				if rec.Body.String() != "user-1" {
					t.Fatalf("subject = %q, want user-1", rec.Body)
				}
				return
			}
			wa := rec.Header().Get("WWW-Authenticate")
			if !strings.HasPrefix(wa, `Bearer realm="goapi"`) {
				t.Fatalf("WWW-Authenticate = %q", wa)
			}
			if got := strings.Contains(wa, `error="invalid_token"`); got != tc.wantInvalid {
				t.Fatalf("WWW-Authenticate = %q, want invalid_token=%v", wa, tc.wantInvalid)
			}
		})
	}
}

func TestRolesAndScopes(t *testing.T) {
	iss := authtest.New(t)
	cases := []struct {
		name       string
		rolesClaim string
		claims     map[string]any
		wantRoles  []string
		wantScopes []string
	}{
		{"top-level array", "roles", map[string]any{"roles": []string{"a", "b"}}, []string{"a", "b"}, nil},
		{"keycloak realm roles", "realm_access.roles",
			map[string]any{"realm_access": map[string]any{"roles": []string{"a"}}}, []string{"a"}, nil},
		{"auth0 namespaced claim", "https://example.com/roles",
			map[string]any{"https://example.com/roles": []string{"a"}}, []string{"a"}, nil},
		{"space separated string", "roles", map[string]any{"roles": "a b"}, []string{"a", "b"}, nil},
		{"missing claim", "roles", nil, []string{}, nil},
		{"scope string", "roles", map[string]any{"scope": "read write"}, []string{}, []string{"read", "write"}},
		{"scp array", "roles", map[string]any{"scp": []string{"read"}}, []string{}, []string{"read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := iss.Verifier(tc.rolesClaim).Verify(context.Background(), iss.Token(t, tc.claims))
			if err != nil {
				t.Fatal(err)
			}
			if p.Roles == nil {
				t.Fatal("Roles must not be nil (JSON should be [] not null)")
			}
			if !slices.Equal(p.Roles, tc.wantRoles) || !slices.Equal(p.Scopes, tc.wantScopes) {
				t.Fatalf("roles=%v scopes=%v, want %v %v", p.Roles, p.Scopes, tc.wantRoles, tc.wantScopes)
			}
		})
	}
}

func TestRequireAnyRole(t *testing.T) {
	h := auth.RequireAnyRole("admin", "editor")(echoSubject)
	cases := []struct {
		name string
		p    *auth.Principal
		want int
	}{
		{"no principal", nil, 401},
		{"no matching role", &auth.Principal{Subject: "u", Roles: []string{"viewer"}}, 403},
		{"one matching role", &auth.Principal{Subject: "u", Roles: []string{"viewer", "editor"}}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.p != nil {
				req = req.WithContext(auth.WithPrincipal(req.Context(), *tc.p))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestWithPrincipalSetsActor(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Subject: "alice"})
	if got := domain.ActorFrom(ctx); got != "alice" {
		t.Fatalf("actor = %q, want alice", got)
	}
}

func TestDisabledInjectsDevPrincipal(t *testing.T) {
	h := auth.Disabled(auth.Principal{Subject: "dev"}, discard).Middleware(echoSubject)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || rec.Body.String() != "dev" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body)
	}
}

type failingVerifier struct{ err error }

func (f failingVerifier) Verify(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, f.err
}

func TestMiddlewareReturns503WhenKeysUnavailable(t *testing.T) {
	err := errors.Join(auth.ErrKeysUnavailable, errors.New("get keys failed: 502"))
	h := auth.New(failingVerifier{err}, discard).Middleware(echoSubject)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer x.y.z")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d Retry-After=%q, want 503 and 5", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("503 must not carry WWW-Authenticate: the token was not judged invalid")
	}
}

// 用 httptest 模拟身份提供方的 JWKS 地址，验证"拉不到公钥"和"token 无效"能区分开。
func TestRemoteVerifierDistinguishesKeyOutageFromBadToken(t *testing.T) {
	iss := authtest.New(t)
	other := authtest.New(t) // 同样的 kid，不同的私钥：缓存里的公钥验不过，会触发重新拉取
	var down atomic.Bool
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(iss.JWKS())
	}))
	defer jwks.Close()

	v := auth.NewRemoteVerifier(authtest.IssuerURL, jwks.URL, authtest.Audience, "roles")
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).Unix()

	// 正常：拉到公钥并缓存
	if _, err := v.Verify(ctx, iss.Token(t, nil)); err != nil {
		t.Fatalf("valid token: %v", err)
	}

	down.Store(true)
	// 公钥地址挂了，但缓存里的公钥仍然能验证合法 token
	if _, err := v.Verify(ctx, iss.Token(t, nil)); err != nil {
		t.Fatalf("valid token with cached keys while JWKS is down: %v", err)
	}
	// 缓存里的公钥验不过、重新拉取又失败：无法判断，应该是 ErrKeysUnavailable
	if _, err := v.Verify(ctx, other.Token(t, nil)); !errors.Is(err, auth.ErrKeysUnavailable) {
		t.Fatalf("unknown key while JWKS is down: err = %v, want ErrKeysUnavailable", err)
	}
	// 过期 token 在验签之前就被拒绝，不需要公钥：仍然是普通的 401
	if _, err := v.Verify(ctx, iss.Token(t, map[string]any{"exp": past})); err == nil || errors.Is(err, auth.ErrKeysUnavailable) {
		t.Fatalf("expired token while JWKS is down: err = %v, want a plain verification error", err)
	}

	down.Store(false)
	// 恢复后：拉取成功但签名确实不对，是普通的 401
	if _, err := v.Verify(ctx, other.Token(t, nil)); err == nil || errors.Is(err, auth.ErrKeysUnavailable) {
		t.Fatalf("bad signature after recovery: err = %v, want a plain verification error", err)
	}
}

// 模拟本地 docker compose 的情况：discovery 文档放在另一个地址，里面的 issuer 和请求地址不同。
func TestOIDCVerifierWithSeparateDiscoveryURL(t *testing.T) {
	iss := authtest.New(t)
	var base string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = io.WriteString(w, `{"issuer":"`+authtest.IssuerURL+`","jwks_uri":"`+base+`/jwks",`+
				`"authorization_endpoint":"`+base+`/auth","token_endpoint":"`+base+`/token"}`)
		case "/jwks":
			_, _ = w.Write(iss.JWKS())
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	base = idp.URL

	// 不设置 DiscoveryURL：直接用 discovery 地址当 issuer，文档里的 issuer 对不上，应该拒绝
	if _, err := auth.NewOIDCVerifier(auth.OIDCConfig{IssuerURL: idp.URL, Audience: authtest.Audience}); err == nil {
		t.Fatal("issuer mismatch must be rejected when DiscoveryURL is not set")
	}

	v, err := auth.NewOIDCVerifier(auth.OIDCConfig{
		IssuerURL: authtest.IssuerURL, DiscoveryURL: idp.URL, Audience: authtest.Audience, RolesClaim: "roles",
	})
	if err != nil {
		t.Fatalf("separate discovery URL: %v", err)
	}
	ctx := context.Background()
	if _, err := v.Verify(ctx, iss.Token(t, nil)); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	// iss 仍然按 IssuerURL 校验：iss 写成 discovery 地址的 token 要拒绝
	if _, err := v.Verify(ctx, iss.Token(t, map[string]any{"iss": idp.URL})); err == nil {
		t.Fatal("token whose iss is the discovery URL must be rejected")
	}
}
