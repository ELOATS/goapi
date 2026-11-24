package consumer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"ELOATS/goapi/internal/store"
)

// 集成测试：设置 TEST_DATABASE_URL 才会运行，会清空本包用到的表。
// 和其它包的集成测试共用数据库，要用 go test -p 1 串行运行（make test-integration 已设置）。

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

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
	if _, err := pool.Exec(ctx, "TRUNCATE processed_events, consumer_dead_letters, welcome_notifications"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type fakeMsg struct {
	subject   string
	data      []byte
	hdr       nats.Header
	delivered uint64

	acked, naked, termed bool
	nakDelay             time.Duration
	inProgress           atomic.Int32
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.delivered}, nil
}
func (m *fakeMsg) Data() []byte         { return m.data }
func (m *fakeMsg) Headers() nats.Header { return m.hdr }
func (m *fakeMsg) Subject() string      { return m.subject }
func (m *fakeMsg) Ack() error           { m.acked = true; return nil }
func (m *fakeMsg) Nak() error           { m.naked = true; return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.naked, m.nakDelay = true, d
	return nil
}
func (m *fakeMsg) InProgress() error { m.inProgress.Add(1); return nil }
func (m *fakeMsg) Term() error       { m.termed = true; return nil }

const userID = "11111111-1111-4111-8111-111111111111"

func newMsg(id int64, typ, payload string, delivered uint64) *fakeMsg {
	h := nats.Header{}
	h.Set("Event-Id", strconv.FormatInt(id, 10))
	h.Set("Event-Type", typ)
	h.Set("Occurred-At", time.Now().UTC().Format(time.RFC3339Nano))
	return &fakeMsg{subject: "goapi.events." + typ, data: []byte(payload), hdr: h, delivered: delivered}
}

const userPayload = `{"id":"` + userID + `","name":"Alice","email":"alice@example.com"}`

func newProcessor(pool *pgxpool.Pool, handlers map[string]Handler, opt ProcessorOptions) *Processor {
	if opt.MaxDeliver == 0 {
		opt.MaxDeliver = 3
	}
	if opt.HandlerTimeout == 0 {
		opt.HandlerTimeout = 5 * time.Second
	}
	return NewProcessor(pool, "test", handlers, opt, discard, nil)
}

func TestProcessThenDuplicate(t *testing.T) {
	pool := newTestDB(t)
	var calls atomic.Int32
	p := newProcessor(pool, map[string]Handler{"user.created": func(ctx context.Context, db DB, e Event) error {
		calls.Add(1)
		return WelcomeHandler(discard)(ctx, db, e)
	}}, ProcessorOptions{})

	first := newMsg(1, "user.created", userPayload, 1)
	if o := p.Process(context.Background(), first); o != OutcomeProcessed || !first.acked {
		t.Fatalf("first: outcome=%s acked=%v", o, first.acked)
	}
	// 同一事件再来一次（例如确认丢失后的重投）
	second := newMsg(1, "user.created", userPayload, 2)
	if o := p.Process(context.Background(), second); o != OutcomeDuplicate || !second.acked {
		t.Fatalf("second: outcome=%s acked=%v", o, second.acked)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", calls.Load())
	}
	if n := count(t, pool, `SELECT count(*) FROM welcome_notifications`); n != 1 {
		t.Fatalf("welcome rows = %d, want 1", n)
	}
}

func TestTransientErrorRollsBackAndRetries(t *testing.T) {
	pool := newTestDB(t)
	p := newProcessor(pool, map[string]Handler{"user.created": func(ctx context.Context, db DB, e Event) error {
		if err := WelcomeHandler(discard)(ctx, db, e); err != nil {
			return err
		}
		return errors.New("downstream timeout") // 业务写入之后才失败
	}}, ProcessorOptions{BackoffBase: time.Second})

	m := newMsg(2, "user.created", userPayload, 2)
	if o := p.Process(context.Background(), m); o != OutcomeRetry {
		t.Fatalf("outcome = %s, want retry", o)
	}
	if !m.naked || m.nakDelay != 2*time.Second || m.acked || m.termed {
		t.Fatalf("settle: naked=%v delay=%v acked=%v termed=%v", m.naked, m.nakDelay, m.acked, m.termed)
	}
	if count(t, pool, `SELECT count(*) FROM welcome_notifications`) != 0 ||
		count(t, pool, `SELECT count(*) FROM processed_events`) != 0 {
		t.Fatal("transaction was not rolled back")
	}
}

func TestLastAttemptGoesToDeadLetter(t *testing.T) {
	pool := newTestDB(t)
	p := newProcessor(pool, map[string]Handler{"user.created": func(context.Context, DB, Event) error {
		return errors.New("still failing")
	}}, ProcessorOptions{MaxDeliver: 3})

	m := newMsg(3, "user.created", userPayload, 3)
	if o := p.Process(context.Background(), m); o != OutcomeDead || !m.termed {
		t.Fatalf("outcome=%s termed=%v", o, m.termed)
	}
	var attempts int
	var errText string
	if err := pool.QueryRow(context.Background(), `SELECT attempts, error FROM consumer_dead_letters
		WHERE consumer = 'test' AND event_id = 3`).Scan(&attempts, &errText); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || errText != "still failing" {
		t.Fatalf("dead letter attempts=%d error=%q", attempts, errText)
	}
}

func TestPermanentErrorGoesToDeadLetterImmediately(t *testing.T) {
	pool := newTestDB(t)
	p := newProcessor(pool, map[string]Handler{"user.created": WelcomeHandler(discard)}, ProcessorOptions{MaxDeliver: 10})

	for i, payload := range []string{
		`not json`,
		`{"id":"","email":""}`,
		`{"id":"not-a-uuid","email":"a@example.com"}`, // Postgres 22P02 → 永久错误
	} {
		m := newMsg(int64(10+i), "user.created", payload, 1)
		if o := p.Process(context.Background(), m); o != OutcomeDead || !m.termed {
			t.Fatalf("payload %q: outcome=%s termed=%v", payload, o, m.termed)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM consumer_dead_letters`); n != 3 {
		t.Fatalf("dead letters = %d, want 3", n)
	}
}

func TestMalformedAndUnknownMessages(t *testing.T) {
	pool := newTestDB(t)
	var calls atomic.Int32
	p := newProcessor(pool, map[string]Handler{"user.created": func(context.Context, DB, Event) error {
		calls.Add(1)
		return nil
	}}, ProcessorOptions{})

	noID := &fakeMsg{subject: "goapi.events.user.created", hdr: nats.Header{}, delivered: 1}
	noID.hdr.Set("Event-Type", "user.created")
	if o := p.Process(context.Background(), noID); o != OutcomeMalformed || !noID.termed {
		t.Fatalf("malformed: outcome=%s termed=%v", o, noID.termed)
	}

	other := newMsg(20, "order.placed", `{}`, 1)
	if o := p.Process(context.Background(), other); o != OutcomeSkipped || !other.acked {
		t.Fatalf("unknown type: outcome=%s acked=%v", o, other.acked)
	}
	if calls.Load() != 0 || count(t, pool, `SELECT count(*) FROM consumer_dead_letters`) != 0 {
		t.Fatal("handler must not run and nothing should be dead-lettered")
	}
}

func TestPanicIsRetried(t *testing.T) {
	pool := newTestDB(t)
	p := newProcessor(pool, map[string]Handler{"user.created": func(context.Context, DB, Event) error {
		panic("boom")
	}}, ProcessorOptions{})
	m := newMsg(30, "user.created", userPayload, 1)
	if o := p.Process(context.Background(), m); o != OutcomeRetry || !m.naked {
		t.Fatalf("outcome=%s naked=%v", o, m.naked)
	}
}

func TestHeartbeatDuringSlowHandler(t *testing.T) {
	pool := newTestDB(t)
	p := newProcessor(pool, map[string]Handler{"user.created": func(context.Context, DB, Event) error {
		time.Sleep(2200 * time.Millisecond)
		return nil
	}}, ProcessorOptions{AckWait: 3 * time.Second}) // 心跳间隔 1s
	m := newMsg(40, "user.created", userPayload, 1)
	if o := p.Process(context.Background(), m); o != OutcomeProcessed {
		t.Fatalf("outcome = %s", o)
	}
	if n := m.inProgress.Load(); n < 2 {
		t.Fatalf("in-progress heartbeats = %d, want >= 2", n)
	}
}

func TestTraceContextReachesHandler(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	pool := newTestDB(t)
	var got trace.TraceID
	p := newProcessor(pool, map[string]Handler{"user.created": func(ctx context.Context, _ DB, _ Event) error {
		got = trace.SpanContextFromContext(ctx).TraceID()
		return nil
	}}, ProcessorOptions{})

	m := newMsg(50, "user.created", userPayload, 1)
	m.hdr.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	p.Process(context.Background(), m)
	if got.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("handler trace id = %s", got)
	}
}

func TestCleanupProcessed(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO processed_events (consumer, event_id, processed_at) VALUES
		('test', 1, now() - interval '30 days'),
		('test', 2, now()),
		('other', 3, now() - interval '30 days')`); err != nil {
		t.Fatal(err)
	}
	n, err := CleanupProcessed(ctx, pool, "test", 14*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v, want 1", n, err)
	}
	if left := count(t, pool, `SELECT count(*) FROM processed_events`); left != 2 {
		t.Fatalf("remaining = %d, want 2 (recent one and the other consumer's)", left)
	}
}
