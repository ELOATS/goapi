package ratelimit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 集成测试：设置 TEST_REDIS_URL 才会运行（make test-integration 会设置）。
// 每个测试用独立的键前缀，并在结束时删除自己的键，不会清空整个库。
func newTestRedis(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("invalid TEST_REDIS_URL")
	}
	c := redis.NewClient(opt)
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("goapi-test:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, err := c.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			c.Del(ctx, keys...)
		}
		c.Close()
	})
	return c, prefix
}

func TestRedisLimiter(t *testing.T) {
	c, prefix := newTestRedis(t)
	ctx := context.Background()
	l, err := NewRedis(c, Policy{Name: "p", Rate: 1, Burst: 3}, prefix)
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range []int{2, 1, 0} {
		r := allow(t, l, "k")
		if !r.Allowed || r.Remaining != want {
			t.Fatalf("request %d: %+v, want allowed with remaining %d", i+1, r, want)
		}
	}
	r := allow(t, l, "k")
	if r.Allowed || r.RetryAfter <= 0 || r.RetryAfter > time.Second {
		t.Fatalf("4th request: %+v, want denied with 0 < RetryAfter <= 1s", r)
	}
	if !allow(t, l, "other").Allowed {
		t.Fatal("other key should have its own quota")
	}

	ttl, err := c.PTTL(ctx, prefix+"p:k").Result()
	if err != nil || ttl <= 0 || ttl > 3*time.Second {
		t.Fatalf("key TTL = %v (err %v), want 0 < ttl <= 3s", ttl, err)
	}
}

func TestRedisLimiterConcurrent(t *testing.T) {
	c, prefix := newTestRedis(t)
	l, err := NewRedis(c, Policy{Name: "p", Rate: 0.01, Burst: 20}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Allow(context.Background(), "k")
			if err != nil {
				t.Error(err)
				return
			}
			if r.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 20 {
		t.Fatalf("allowed = %d, want exactly 20", got)
	}
}
