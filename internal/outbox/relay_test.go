package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"ELOATS/goapi/internal/domain"
	"ELOATS/goapi/internal/store"
)

// 集成测试：设置 TEST_DATABASE_URL 才会运行，会清空 outbox_events 表，不要指向生产库。
// 和 internal/store 的测试共用数据库，要用 go test -p 1 串行运行（make test-integration 已设置）。

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

const aggID = "00000000-0000-0000-0000-000000000001"

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, store.PoolConfig{URL: url, MaxConns: 6, MinConns: 0,
		MaxConnLifetime: time.Minute, MaxConnIdleTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func insertEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool, types ...string) {
	t.Helper()
	es := store.NewPostgresEventStore(pool, 5*time.Second)
	for _, typ := range types {
		if err := es.Append(ctx, domain.Event{AggregateID: aggID, Type: typ, Payload: []byte(`{"ok":true}`), Actor: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
}

type eventState struct {
	published, dead bool
	attempts        int
	lastError       string
}

func states(t *testing.T, pool *pgxpool.Pool) []eventState {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT published_at IS NOT NULL, dead_at IS NOT NULL,
		attempts, coalesce(last_error, '') FROM outbox_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []eventState
	for rows.Next() {
		var s eventState
		if err := rows.Scan(&s.published, &s.dead, &s.attempts, &s.lastError); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

type fakePublisher struct {
	mu    sync.Mutex
	got   []Message
	ctxs  []context.Context
	fail  func(Message) error
	delay time.Duration
}

func (f *fakePublisher) Publish(ctx context.Context, m Message) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		if err := f.fail(m); err != nil {
			return err
		}
	}
	f.got = append(f.got, m)
	f.ctxs = append(f.ctxs, ctx)
	return nil
}

func (f *fakePublisher) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int64
	for _, m := range f.got {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestRelayPublishesInOrderAndMarks(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	insertEvents(ctx, t, pool, "a.one", "a.two", "a.three")

	pub := &fakePublisher{}
	r := New(pool, pub, Options{BatchSize: 10}, discard, nil)
	n, err := r.ProcessBatch(ctx)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v, want 3 nil", n, err)
	}
	if ids := pub.ids(); !slices.IsSorted(ids) || len(ids) != 3 {
		t.Fatalf("published ids = %v, want 3 in ascending order", ids)
	}
	if m := pub.got[0]; m.Type != "a.one" || m.Actor != "alice" || m.AggregateID != aggID || string(m.Payload) != `{"ok": true}` && string(m.Payload) != `{"ok":true}` {
		t.Fatalf("unexpected message %+v", m)
	}
	for i, s := range states(t, pool) {
		if !s.published {
			t.Fatalf("event %d not marked published", i)
		}
	}
	if n, err := r.ProcessBatch(ctx); n != 0 || err != nil {
		t.Fatalf("second batch n=%d err=%v, want 0 nil", n, err)
	}
}

func TestRelayPropagatesTraceContext(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	pool := newTestDB(t)
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	reqCtx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))
	insertEvents(reqCtx, t, pool, "a.traced")

	pub := &fakePublisher{}
	if _, err := New(pool, pub, Options{}, discard, nil).ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.ctxs) != 1 {
		t.Fatalf("published %d messages, want 1", len(pub.ctxs))
	}
	if got := trace.SpanContextFromContext(pub.ctxs[0]).TraceID(); got != tid {
		t.Fatalf("publish trace id = %s, want %s", got, tid)
	}
}

func TestRelayTransientFailureStopsBatch(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	insertEvents(ctx, t, pool, "a.one", "a.two", "a.three")

	down := true
	pub := &fakePublisher{fail: func(m Message) error {
		if down && m.Type == "a.two" {
			return errors.New("nats: timeout")
		}
		return nil
	}}
	r := New(pool, pub, Options{}, discard, nil)

	if _, err := r.ProcessBatch(ctx); err == nil {
		t.Fatal("want error from transient failure")
	}
	s := states(t, pool)
	if !s[0].published || s[1].published || s[1].dead || s[1].attempts != 1 || s[1].lastError == "" || s[2].published || s[2].attempts != 0 {
		t.Fatalf("states after transient failure = %+v", s)
	}

	down = false
	if _, err := r.ProcessBatch(ctx); err != nil {
		t.Fatal(err)
	}
	for i, st := range states(t, pool) {
		if !st.published || st.lastError != "" {
			t.Fatalf("event %d after recovery: %+v", i, st)
		}
	}
}

func TestRelayPermanentFailureGoesToDeadLetter(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	insertEvents(ctx, t, pool, "a.one", "a.bad", "a.three")

	pub := &fakePublisher{fail: func(m Message) error {
		if m.Type == "a.bad" {
			return Permanent(errors.New("payload too large"))
		}
		return nil
	}}
	r := New(pool, pub, Options{}, discard, nil)
	if _, err := r.ProcessBatch(ctx); err != nil {
		t.Fatalf("permanent failures must not fail the batch: %v", err)
	}
	s := states(t, pool)
	if !s[0].published || !s[1].dead || s[1].published || !s[2].published {
		t.Fatalf("states = %+v", s)
	}
	if n, _ := r.ProcessBatch(ctx); n != 0 {
		t.Fatalf("dead events must not be picked up again, got %d", n)
	}
}

func TestRelaysDoNotPublishTwice(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	types := make([]string, 40)
	for i := range types {
		types[i] = "a.bulk"
	}
	insertEvents(ctx, t, pool, types...)

	pub := &fakePublisher{delay: 2 * time.Millisecond}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(pool, pub, Options{BatchSize: 5}, discard, nil)
			for {
				n, err := r.ProcessBatch(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	ids := pub.ids()
	slices.Sort(ids)
	if len(ids) != 40 || len(slices.Compact(slices.Clone(ids))) != 40 {
		t.Fatalf("published %d messages (%d unique), want 40 unique", len(ids), len(slices.Compact(ids)))
	}
	for i, s := range states(t, pool) {
		if !s.published {
			t.Fatalf("event %d left unpublished", i)
		}
	}
}

func TestCleanupDeletesOnlyOldPublished(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	insertEvents(ctx, t, pool, "a.old", "a.recent", "a.pending")
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET published_at = now() - interval '10 days' WHERE event_type = 'a.old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE event_type = 'a.recent'`); err != nil {
		t.Fatal(err)
	}

	r := New(pool, &fakePublisher{}, Options{Retention: 7 * 24 * time.Hour}, discard, nil)
	n, err := r.Cleanup(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v, want 1 nil", n, err)
	}
	var left []string
	rows, _ := pool.Query(ctx, `SELECT event_type FROM outbox_events ORDER BY id`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		left = append(left, s)
	}
	if !slices.Equal(left, []string{"a.recent", "a.pending"}) {
		t.Fatalf("remaining = %v", left)
	}
}

func TestListenWakesOnInsert(t *testing.T) {
	pool := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New(pool, &fakePublisher{}, Options{}, discard, nil)
	wake := make(chan struct{}, 1)
	go r.listen(ctx, wake)

	// LISTEN 生效前插入的通知会丢，所以多插几次直到收到
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-wake:
			return
		case <-tick.C:
			insertEvents(context.Background(), t, pool, "a.notify")
		case <-deadline:
			t.Fatal("no notification within 5s")
		}
	}
}
