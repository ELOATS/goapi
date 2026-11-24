package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/go-chi/chi/v5"

	apispec "example.com/goapi/api"
	"example.com/goapi/internal/auth"
	"example.com/goapi/internal/auth/authtest"
	"example.com/goapi/internal/config"
	"example.com/goapi/internal/ratelimit"
	"example.com/goapi/internal/service"
	"example.com/goapi/internal/store"
)

// 文档自身的路由，不写进 openapi.yaml
var docsRoutes = map[string]bool{"GET /api/openapi.yaml": true, "GET /api/docs": true}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
	return doc
}

// specRouter 按规范匹配请求。去掉 servers：测试请求的 host 不是规范里写的地址。
func specRouter(t *testing.T, doc *openapi3.T) routers.Router {
	t.Helper()
	doc.Servers = nil
	r, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var contractOpts = &openapi3filter.Options{
	AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc, // 认证由真实中间件负责，这里只校验结构
	IncludeResponseStatus: true,                                  // 规范里没写的状态码算不符合
	MultiError:            true,
}

// checkContract 校验一次请求和它的响应是否符合规范，返回匹配到的 operationId。
// newReq 每次返回一个新的、相同的请求（请求体只能读一次）。
func checkContract(t *testing.T, router routers.Router, newReq func() *http.Request, rec *httptest.ResponseRecorder, validateRequest bool) string {
	t.Helper()
	req := newReq()
	route, params, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s %s is not documented in api/openapi.yaml: %v", req.Method, req.URL.Path, err)
	}
	ctx := context.Background()
	in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route, Options: contractOpts}
	if validateRequest {
		if err := openapi3filter.ValidateRequest(ctx, in); err != nil {
			t.Fatalf("request does not match spec: %v", err)
		}
	}
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in,
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options:                contractOpts,
	}
	if err := openapi3filter.ValidateResponse(ctx, out); err != nil {
		t.Fatalf("response %d does not match spec: %v\nbody: %s", rec.Code, err, rec.Body)
	}
	return route.Operation.OperationID
}

func newContractRouter(t *testing.T, iss *authtest.Issuer, userLimiter ratelimit.Limiter) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{HandlerTimeout: 2 * time.Second, MaxBodyBytes: 1 << 10}
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	return NewRouter(cfg, logger, Deps{
		Users:         svc,
		Auth:          auth.New(iss.Verifier("roles"), logger),
		RateLimitUser: userLimiter,
	}, func() bool { return true })
}

// TestOpenAPIContract 用真实的路由、中间件和处理器发请求，逐条按 api/openapi.yaml 校验，
// 并要求规范里的每个接口至少被覆盖一次。处理器改了响应结构、加了状态码，而规范没跟着改，这里就会失败。
func TestOpenAPIContract(t *testing.T) {
	doc := loadSpec(t)
	router := specRouter(t, doc)
	iss := authtest.New(t)
	h := newContractRouter(t, iss, nil)

	admin := iss.Token(t, map[string]any{"sub": "admin-1", "roles": []string{RoleAdmin}})
	viewer := iss.Token(t, map[string]any{"sub": "viewer-1", "roles": []string{RoleViewer}})
	nobody := iss.Token(t, map[string]any{"sub": "nobody-1", "email": "n@example.com"})
	alice := `{"name":"Alice","email":"alice@example.com"}`

	cases := []struct {
		name, method, path, token, body, contentType string
		want                                          int
		invalidRequest                                bool // 故意发不符合规范的请求，只校验响应
	}{
		{name: "healthz", method: "GET", path: "/healthz", want: 200},
		{name: "readyz", method: "GET", path: "/readyz", want: 200},
		{name: "me", method: "GET", path: "/api/v1/me", token: nobody, want: 200},
		{name: "me without token", method: "GET", path: "/api/v1/me", want: 401},
		{name: "me with bad token", method: "GET", path: "/api/v1/me", token: "not-a-jwt", want: 401},
		{name: "create", method: "POST", path: "/api/v1/users", token: admin, body: alice, want: 201},
		{name: "create duplicate", method: "POST", path: "/api/v1/users", token: admin,
			body: `{"name":"Alice","email":"ALICE@example.com"}`, want: 409},
		{name: "create as viewer", method: "POST", path: "/api/v1/users", token: viewer, body: alice, want: 403},
		{name: "create with unknown field", method: "POST", path: "/api/v1/users", token: admin,
			body: `{"name":"B","email":"b@example.com","admin":true}`, want: 400, invalidRequest: true},
		{name: "create with invalid email", method: "POST", path: "/api/v1/users", token: admin,
			body: `{"name":"B","email":"nope"}`, want: 422, invalidRequest: true},
		{name: "create with wrong content type", method: "POST", path: "/api/v1/users", token: admin,
			body: "hello", contentType: "text/plain", want: 415, invalidRequest: true},
		{name: "list", method: "GET", path: "/api/v1/users?limit=1", token: viewer, want: 200},
		{name: "list with bad limit", method: "GET", path: "/api/v1/users?limit=0", token: viewer, want: 400, invalidRequest: true},
		{name: "list with bad cursor", method: "GET", path: "/api/v1/users?cursor=e30", token: viewer, want: 400},
		{name: "list without role", method: "GET", path: "/api/v1/users", token: nobody, want: 403},
		{name: "get", method: "GET", path: "/api/v1/users/{created}", token: viewer, want: 200},
		{name: "get missing", method: "GET", path: "/api/v1/users/00000000-0000-0000-0000-000000000000", token: viewer, want: 404},
	}

	covered := map[string]bool{}
	var createdID string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := strings.ReplaceAll(tc.path, "{created}", createdID)
			newReq := func() *http.Request {
				req := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
				ct := tc.contentType
				if ct == "" && tc.body != "" {
					ct = "application/json"
				}
				if ct != "" {
					req.Header.Set("Content-Type", ct)
				}
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				return req
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newReq())
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			covered[checkContract(t, router, newReq, rec, !tc.invalidRequest)] = true

			if tc.name == "create" {
				var u struct{ ID string }
				if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil || u.ID == "" {
					t.Fatalf("cannot read created id: %v", err)
				}
				createdID = u.ID
			}
		})
	}

	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !covered[op.OperationID] {
				t.Errorf("%s %s (%s) is documented but not exercised by this test", method, path, op.OperationID)
			}
		}
	}
}

func TestOpenAPIContractRateLimited(t *testing.T) {
	router := specRouter(t, loadSpec(t))
	iss := authtest.New(t)
	limiter, err := ratelimit.NewMemory(ratelimit.Policy{Name: "user", Rate: 0.001, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := newContractRouter(t, iss, limiter)
	token := iss.Token(t, map[string]any{"sub": "u", "roles": []string{RoleViewer}})
	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}

	h.ServeHTTP(httptest.NewRecorder(), newReq())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	checkContract(t, router, newReq, rec, true)
}

// TestRoutesMatchSpec 要求路由表和规范一一对应：加了路由没写文档，或者文档里的接口已经删了，都会失败。
func TestRoutesMatchSpec(t *testing.T) {
	doc := loadSpec(t)
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	cfg := config.Config{HandlerTimeout: time.Second, MaxBodyBytes: 1 << 10, DocsEnabled: true}
	svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
	h := NewRouter(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{Users: svc, Auth: testAuth()}, func() bool { return true })
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T, want a chi router", h)
	}

	registered := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route != "/" {
			route = strings.TrimSuffix(route, "/") // 子路由里的 Get("/") 会得到 /api/v1/users/
		}
		if key := method + " " + route; !docsRoutes[key] {
			registered[key] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var missing, stale []string
	for k := range registered {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !registered[k] {
			stale = append(stale, k)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	if len(missing) > 0 {
		t.Errorf("routes not documented in api/openapi.yaml: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("documented in api/openapi.yaml but no such route: %v", stale)
	}
}

func TestDocsRoutes(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := config.Config{HandlerTimeout: time.Second, MaxBodyBytes: 1 << 10, DocsEnabled: enabled}
		svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
		h := NewRouter(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{Users: svc, Auth: testAuth()}, func() bool { return true })
		want := http.StatusNotFound
		if enabled {
			want = http.StatusOK
		}
		for _, p := range []string{"/api/openapi.yaml", "/api/docs"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			if rec.Code != want {
				t.Errorf("DocsEnabled=%v GET %s = %d, want %d", enabled, p, rec.Code, want)
			}
		}
	}
}
