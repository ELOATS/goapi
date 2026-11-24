package consumer

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
)

func TestParseEvent(t *testing.T) {
	full := nats.Header{}
	full.Set("Event-Id", "42")
	full.Set("Event-Type", "user.created")
	full.Set("Aggregate-Id", "agg")
	full.Set("Actor", "alice")
	full.Set("Occurred-At", "2026-11-23T04:00:00.123Z")

	e, err := ParseEvent("goapi.events.user.created", full, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != 42 || e.Type != "user.created" || e.AggregateID != "agg" || e.Actor != "alice" ||
		e.OccurredAt.IsZero() || e.Subject != "goapi.events.user.created" {
		t.Fatalf("unexpected event %+v", e)
	}

	bad := map[string]map[string]string{
		"no id":         {"Event-Type": "x"},
		"non-numeric":   {"Event-Id": "abc", "Event-Type": "x"},
		"zero id":       {"Event-Id": "0", "Event-Type": "x"},
		"no type":       {"Event-Id": "1"},
		"bad timestamp": {"Event-Id": "1", "Event-Type": "x", "Occurred-At": "yesterday"},
	}
	for name, hs := range bad {
		h := nats.Header{}
		for k, v := range hs {
			h.Set(k, v)
		}
		if _, err := ParseEvent("s", h, nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ParseEvent("s", nil, nil); err == nil {
		t.Error("nil headers: want error")
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range want {
		if got := Backoff(uint64(i+1), time.Second, 5*time.Minute); got != w*time.Second {
			t.Fatalf("Backoff(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if Backoff(0, time.Second, time.Minute) != time.Second {
		t.Fatal("attempt 0 should be treated as 1")
	}
}

func TestPermanentClassification(t *testing.T) {
	base := errors.New("bad payload")
	if !IsPermanent(fmt.Errorf("wrap: %w", Permanent(base))) || IsPermanent(base) || Permanent(nil) != nil {
		t.Fatal("Permanent / IsPermanent")
	}
	for code, want := range map[string]bool{"22P02": true, "23505": true, "40001": false, "57P01": false} {
		err := fmt.Errorf("insert: %w", &pgconn.PgError{Code: code})
		if got := IsPermanent(PermanentIfDataError(err)); got != want {
			t.Errorf("code %s permanent = %v, want %v", code, got, want)
		}
	}
	if IsPermanent(PermanentIfDataError(errors.New("connection refused"))) {
		t.Error("network errors must stay transient")
	}
}
