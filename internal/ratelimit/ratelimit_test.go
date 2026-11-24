package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestMemory(t *testing.T, p Policy) (*Memory, *fakeClock) {
	t.Helper()
	m, err := NewMemory(p)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	m.now = c.Now
	m.lastSweep = c.Now()
	return m, c
}

func allow(t *testing.T, l Limiter, key string) Result {
	t.Helper()
	r, err := l.Allow(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGCRABurstRefill(t *testing.T) {
	m, clock := newTestMemory(t, Policy{Name: "p", Rate: 1, Burst: 3})

	for i, want := range []int{2, 1, 0} {
		r := allow(t, m, "k")
		if !r.Allowed || r.Remaining != want || r.Limit != 3 {
			t.Fatalf("request %d: %+v, want allowed with remaining %d", i+1, r, want)
		}
	}

	r := allow(t, m, "k")
	if r.Allowed || r.RetryAfter != time.Second || r.ResetAfter != 3*time.Second {
		t.Fatalf("4th request: %+v, want denied, retry after 1s, reset after 3s", r)
	}
	// 被拒绝的请求不消耗额度：再试一次结果相同
	if r2 := allow(t, m, "k"); r2.Allowed || r2.RetryAfter != time.Second {
		t.Fatalf("denied request consumed quota: %+v", r2)
	}

	clock.Advance(time.Second)
	if r := allow(t, m, "k"); !r.Allowed || r.Remaining != 0 {
		t.Fatalf("after 1s: %+v, want one more allowed", r)
	}

	clock.Advance(time.Hour)
	if r := allow(t, m, "k"); !r.Allowed || r.Remaining != 2 {
		t.Fatalf("after full refill: %+v, want remaining 2", r)
	}
}

func TestMemoryKeysAreIndependent(t *testing.T) {
	m, _ := newTestMemory(t, Policy{Name: "p", Rate: 1, Burst: 1})
	if !allow(t, m, "a").Allowed || allow(t, m, "a").Allowed {
		t.Fatal("key a: want first allowed, second denied")
	}
	if !allow(t, m, "b").Allowed {
		t.Fatal("key b should have its own quota")
	}
}

func TestMemoryConcurrent(t *testing.T) {
	m, _ := newTestMemory(t, Policy{Name: "p", Rate: 0.001, Burst: 50})
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, _ := m.Allow(context.Background(), "k"); r.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 50 {
		t.Fatalf("allowed = %d, want exactly 50", got)
	}
}

func TestMemorySweepsRefilledKeys(t *testing.T) {
	m, clock := newTestMemory(t, Policy{Name: "p", Rate: 1, Burst: 5})
	allow(t, m, "a")
	allow(t, m, "b")
	clock.Advance(2 * time.Minute)
	allow(t, m, "c")
	if n := m.Len(); n != 1 {
		t.Fatalf("tracked keys = %d, want 1 (a and b should be swept)", n)
	}
}

func TestPolicyValidate(t *testing.T) {
	bad := []Policy{
		{Name: "", Rate: 1, Burst: 1},
		{Name: "p", Rate: 0, Burst: 1},
		{Name: "p", Rate: -1, Burst: 1},
		{Name: "p", Rate: 2e6, Burst: 1},
		{Name: "p", Rate: 1, Burst: 0},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("%+v should be invalid", p)
		}
	}
	if err := (Policy{Name: "p", Rate: 0.5, Burst: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestClientIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7:5555":           "203.0.113.7",
		"203.0.113.7":                "203.0.113.7",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":       "2001:db8:1:2::/64",
		"[::ffff:203.0.113.7]:80":    "203.0.113.7",
		"not-an-ip":                  "not-an-ip",
	}
	for in, want := range cases {
		if got := clientIP(in); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", in, got, want)
		}
	}
}
