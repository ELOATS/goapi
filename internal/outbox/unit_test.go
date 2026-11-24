package outbox

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPermanent(t *testing.T) {
	base := errors.New("too big")
	err := fmt.Errorf("publish: %w", Permanent(base))
	if !IsPermanent(err) || !errors.Is(err, base) {
		t.Fatalf("wrapped permanent error not recognized: %v", err)
	}
	if IsPermanent(base) || Permanent(nil) != nil {
		t.Fatal("plain errors must not be permanent; Permanent(nil) must be nil")
	}
}

func TestValidSubject(t *testing.T) {
	for _, s := range []string{"user.created", "a", "goapi.events", "x-y_z.1"} {
		if !validSubject(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "a..b", ".a", "a.", "a b", "a.*", "a.>", "a\nb"} {
		if validSubject(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	initial, maxDelay := time.Second, 30*time.Second
	var got []time.Duration
	var cur time.Duration
	for range 7 {
		cur = nextBackoff(cur, initial, maxDelay)
		got = append(got, cur)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff sequence = %v", got)
		}
	}
}

func TestHealthy(t *testing.T) {
	r := New(nil, nil, Options{}, nil, nil)
	if !r.Healthy(time.Minute) {
		t.Fatal("new relay should be healthy")
	}
	r.lastLoop.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	if r.Healthy(time.Minute) {
		t.Fatal("stale relay should be unhealthy")
	}
}

func TestMessageIDStable(t *testing.T) {
	a, b := MessageID(42), MessageID(42)
	if a != b || a == MessageID(43) {
		t.Fatal("message id must be stable per event and unique across events")
	}
}
