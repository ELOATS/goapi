package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var (
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
	ok200   = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	byConst = func(*http.Request) (string, bool) { return "k", true }
)

func serve(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestMiddlewareHeadersAnd429(t *testing.T) {
	m, _ := newTestMemory(t, Policy{Name: "p", Rate: 1, Burst: 2})
	metrics := NewMetrics(prometheus.NewRegistry())
	h := Middleware(m, Options{Key: byConst, Metrics: metrics, Logger: discard})(ok200)

	steps := []struct {
		code                      int
		remaining, reset, retryAt string
	}{
		{200, "1", "1", ""},
		{200, "0", "2", ""},
		{429, "0", "2", "1"},
	}
	for i, s := range steps {
		rec := serve(h)
		hd := rec.Header()
		if rec.Code != s.code || hd.Get("RateLimit-Limit") != "2" || hd.Get("RateLimit-Remaining") != s.remaining ||
			hd.Get("RateLimit-Reset") != s.reset || hd.Get("Retry-After") != s.retryAt {
			t.Fatalf("request %d: code=%d headers=%v, want %+v", i+1, rec.Code, hd, s)
		}
	}

	if got := testutil.ToFloat64(metrics.decisions.WithLabelValues("p", "allowed")); got != 2 {
		t.Fatalf("allowed = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.decisions.WithLabelValues("p", "limited")); got != 1 {
		t.Fatalf("limited = %v, want 1", got)
	}
}

func TestMiddlewareSkipsWhenNoKey(t *testing.T) {
	m, _ := newTestMemory(t, Policy{Name: "p", Rate: 1, Burst: 1})
	skip := func(*http.Request) (string, bool) { return "", false }
	h := Middleware(m, Options{Key: skip})(ok200)
	for range 3 {
		if rec := serve(h); rec.Code != 200 || rec.Header().Get("RateLimit-Limit") != "" {
			t.Fatalf("code=%d headers=%v, want 200 without rate limit headers", rec.Code, rec.Header())
		}
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string) (Result, error) {
	return Result{}, errors.New("redis down")
}
func (brokenLimiter) Policy() Policy { return Policy{Name: "broken"} }

func TestMiddlewareFailsOpen(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	h := Middleware(brokenLimiter{}, Options{Key: byConst, Metrics: metrics, Logger: discard})(ok200)
	for range 3 {
		if rec := serve(h); rec.Code != 200 {
			t.Fatalf("code = %d, want 200 (fail open)", rec.Code)
		}
	}
	if got := testutil.ToFloat64(metrics.decisions.WithLabelValues("broken", "error")); got != 3 {
		t.Fatalf("error decisions = %v, want 3", got)
	}
}
