package httpserver

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"example.com/goapi/internal/auth"
	"example.com/goapi/internal/auth/authtest"
	"example.com/goapi/internal/config"
	"example.com/goapi/internal/domain"
	"example.com/goapi/internal/ratelimit"
	"example.com/goapi/internal/service"
	"example.com/goapi/internal/store"
	"example.com/goapi/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, logger, Deps{Users: svc, Auth: testAuth()}, func() bool { return true })
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func postUser(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	res, err := http.Post(ts.URL+"/api/v1/users", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

func TestCreateAndGetUser(t *testing.T) {
	ts := newTestServer(t)

	res := postUser(t, ts, `{"name":"Alice","email":"alice@example.com"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}
	var created domain.User
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	res2, err := http.Get(ts.URL + "/api/v1/users/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", res2.StatusCode)
	}

	dup := postUser(t, ts, `{"name":"Alice","email":"ALICE@example.com"}`)
	defer dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", dup.StatusCode)
	}
}

func TestCreateUserErrors(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown field", `{"name":"A","email":"a@example.com","admin":true}`, http.StatusBadRequest},
		{"malformed", `{"name":`, http.StatusBadRequest},
		{"two objects", `{"name":"A","email":"a@example.com"}{}`, http.StatusBadRequest},
		{"invalid email", `{"name":"A","email":"nope"}`, http.StatusUnprocessableEntity},
		{"too large", `{"name":"` + strings.Repeat("x", 2048) + `","email":"a@example.com"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := postUser(t, ts, tc.body)
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

func TestGetUserNotFound(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/users/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

type listResp struct {
	Items      []domain.User `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

func TestListUsersPagination(t *testing.T) {
	ts := newTestServer(t)
	for _, e := range []string{"a@x.io", "b@x.io", "c@x.io", "d@x.io", "e@x.io"} {
		res := postUser(t, ts, `{"name":"u","email":"`+e+`"}`)
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("seed status = %d", res.StatusCode)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		u := ts.URL + "/api/v1/users?limit=2"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		res, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		var body listResp
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d", res.StatusCode)
		}
		pages++
		for _, it := range body.Items {
			if seen[it.ID] {
				t.Fatalf("duplicate %s", it.ID)
			}
			seen[it.ID] = true
		}
		if body.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		cursor = body.NextCursor
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("seen=%d pages=%d, want 5 and 3", len(seen), pages)
	}
}

func TestListUsersEmptyReturnsArray(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"items":[]`) {
		t.Fatalf("body = %s, want items to be []", b)
	}
}

func TestListUsersBadParams(t *testing.T) {
	ts := newTestServer(t)
	for _, q := range []string{"limit=0", "limit=101", "limit=abc", "cursor=%21%21%21", "cursor=e30", "cursor=eyJ0IjoiMjAyNi0xMS0yNFQwMzowMDowMFoiLCJpZCI6IjExMTExMTExLTExMTEtNDExMS04MTExLTExMTExMTExMTExMSJ9"} {
		t.Run(q, func(t *testing.T) {
			res, err := http.Get(ts.URL + "/api/v1/users?" + q)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
		})
	}
}

func TestMetricsUseRouteTemplate(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, logger, Deps{Users: svc, Metrics: telemetry.NewHTTPMetrics(reg), Auth: testAuth()}, func() bool { return true })
	ts := httptest.NewServer(h)
	defer ts.Close()

	for _, p := range []string{"/api/v1/users/aaa", "/api/v1/users/bbb", "/no/such/path"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{} // "route|status" -> count
	for _, mf := range mfs {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var route, status string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "route":
					route = l.GetValue()
				case "status":
					status = l.GetValue()
				}
			}
			got[route+"|"+status] += m.GetCounter().GetValue()
		}
	}
	if got["/api/v1/users/{id}|404"] != 2 {
		t.Fatalf("want 2 requests under route template, got %v", got)
	}
	if got["unmatched|404"] != 1 {
		t.Fatalf("want unknown path under 'unmatched', got %v", got)
	}
	for k := range got {
		if strings.Contains(k, "aaa") || strings.Contains(k, "bbb") {
			t.Fatalf("raw path leaked into labels: %v", got)
		}
	}
}

// testAuth 关闭认证，所有请求都是管理员：其它测试只关心业务逻辑。
func testAuth() *auth.Authenticator {
	return auth.Disabled(auth.Principal{Subject: "test", Roles: []string{RoleAdmin}}, nil)
}

func TestAuthEnforced(t *testing.T) {
	iss := authtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, logger, Deps{Users: svc, Auth: auth.New(iss.Verifier("roles"), logger)}, func() bool { return true })
	ts := httptest.NewServer(h)
	defer ts.Close()

	viewer := iss.Token(t, map[string]any{"sub": "v-1", "roles": []string{RoleViewer}})
	admin := iss.Token(t, map[string]any{"sub": "a-1", "roles": []string{RoleAdmin}})
	nobody := iss.Token(t, map[string]any{"sub": "n-1"})
	body := `{"name":"A","email":"a@example.com"}`

	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"probes need no token", "GET", "/healthz", "", "", 200},
		{"api needs token", "GET", "/api/v1/users", "", "", 401},
		{"valid token without roles", "GET", "/api/v1/users", nobody, "", 403},
		{"viewer can list", "GET", "/api/v1/users", viewer, "", 200},
		{"viewer cannot create", "POST", "/api/v1/users", viewer, body, 403},
		{"admin can create", "POST", "/api/v1/users", admin, body, 201},
		{"anyone authenticated can call /me", "GET", "/api/v1/me", nobody, "", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("status = %d, want %d (body %s)", res.StatusCode, tc.want, b)
			}
		})
	}
}

func TestRateLimits(t *testing.T) {
	iss := authtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	newServer := func(t *testing.T, ipBurst, userBurst int) *httptest.Server {
		t.Helper()
		ipL, err := ratelimit.NewMemory(ratelimit.Policy{Name: "ip", Rate: 0.001, Burst: ipBurst})
		if err != nil {
			t.Fatal(err)
		}
		userL, err := ratelimit.NewMemory(ratelimit.Policy{Name: "user", Rate: 0.001, Burst: userBurst})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
		svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
		h := NewRouter(cfg, logger, Deps{
			Users:         svc,
			Auth:          auth.New(iss.Verifier("roles"), logger),
			RateLimitIP:   ipL,
			RateLimitUser: userL,
		}, func() bool { return true })
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts
	}
	get := func(t *testing.T, ts *httptest.Server, token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/users", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	t.Run("per user", func(t *testing.T) {
		ts := newServer(t, 100, 2)
		alice := iss.Token(t, map[string]any{"sub": "alice", "roles": []string{RoleViewer}})
		bob := iss.Token(t, map[string]any{"sub": "bob", "roles": []string{RoleViewer}})

		var last *http.Response
		for i, want := range []int{200, 200, 429} {
			last = get(t, ts, alice)
			if last.StatusCode != want {
				t.Fatalf("alice request %d: status %d, want %d", i+1, last.StatusCode, want)
			}
		}
		if last.Header.Get("Retry-After") == "" {
			t.Fatal("429 response must carry Retry-After")
		}
		if res := get(t, ts, bob); res.StatusCode != 200 {
			t.Fatalf("bob status %d, want 200 (separate quota)", res.StatusCode)
		}
	})

	t.Run("per ip runs before auth", func(t *testing.T) {
		ts := newServer(t, 2, 100)
		for i, want := range []int{401, 401, 429} {
			if res := get(t, ts, ""); res.StatusCode != want {
				t.Fatalf("request %d: status %d, want %d", i+1, res.StatusCode, want)
			}
		}
	})

	t.Run("probes are not limited", func(t *testing.T) {
		ts := newServer(t, 1, 1)
		for range 3 {
			res, err := http.Get(ts.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("healthz status %d", res.StatusCode)
			}
		}
	})
}

func TestRealIPOnlyTrustsConfiguredHeader(t *testing.T) {
	var got string
	h := realIP("X-Real-IP")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"configured header", map[string]string{"X-Real-IP": "203.0.113.9"}, "203.0.113.9"},
		{"other headers ignored", map[string]string{"True-Client-IP": "6.6.6.6", "X-Forwarded-For": "6.6.6.6"}, "192.0.2.1:1234"},
		{"invalid value ignored", map[string]string{"X-Real-IP": "6.6.6.6, 7.7.7.7"}, "192.0.2.1:1234"},
	}
	// Envoy Gateway 部署时用的头
	envoy := realIP("X-Envoy-External-Address")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:4321"
	req.Header.Set("X-Real-IP", "6.6.6.6")
	req.Header.Set("X-Envoy-External-Address", "198.51.100.7")
	envoy.ServeHTTP(httptest.NewRecorder(), req)
	if got != "198.51.100.7" {
		t.Fatalf("envoy header: RemoteAddr = %q, want 198.51.100.7", got)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tc.want {
				t.Fatalf("RemoteAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponseCompression(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, logger, Deps{Users: svc, Auth: testAuth()}, func() bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil || !strings.Contains(string(body), `"items"`) {
		t.Fatalf("decompressed body = %q, err = %v", body, err)
	}

	// 客户端没说支持压缩：原样返回
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	if plain.Header().Get("Content-Encoding") != "" || !strings.Contains(plain.Body.String(), `"items"`) {
		t.Fatalf("uncompressed response: encoding=%q body=%q", plain.Header().Get("Content-Encoding"), plain.Body.String())
	}
}

func TestWriteRateLimit(t *testing.T) {
	iss := authtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	userL, err := ratelimit.NewMemory(ratelimit.Policy{Name: "user", Rate: 0.001, Burst: 100})
	if err != nil {
		t.Fatal(err)
	}
	writeL, err := ratelimit.NewMemory(ratelimit.Policy{Name: "write", Rate: 0.001, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, logger, Deps{
		Users:          svc,
		Auth:           auth.New(iss.Verifier("roles"), logger),
		RateLimitUser:  userL,
		RateLimitWrite: writeL,
	}, func() bool { return true })

	admin := iss.Token(t, map[string]any{"sub": "admin-1", "roles": []string{RoleAdmin}})
	viewer := iss.Token(t, map[string]any{"sub": "viewer-1", "roles": []string{RoleViewer}})
	do := func(method, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/users", strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 角色检查在写限流之前：没有权限的请求一直是 403，不会变成 429
	for i := range 2 {
		if rec := do(http.MethodPost, viewer, `{"name":"V","email":"v@example.com"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("viewer POST #%d: status %d, want 403", i+1, rec.Code)
		}
	}
	if rec := do(http.MethodPost, admin, `{"name":"A","email":"a1@example.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first admin POST: status %d, want 201", rec.Code)
	}
	rec := do(http.MethodPost, admin, `{"name":"A","email":"a2@example.com"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second admin POST: status %d Retry-After %q, want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	// 读接口只受按用户的总额度约束，不受写额度影响
	if rec := do(http.MethodGet, admin, ""); rec.Code != http.StatusOK {
		t.Fatalf("admin GET after write limit: status %d, want 200", rec.Code)
	}
}
