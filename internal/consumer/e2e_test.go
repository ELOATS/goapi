package consumer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ELOATS/goapi/internal/outbox"
)

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// 端到端：outbox 的 NATS 发布器 → JetStream → 持久消费者 → 处理器 → Postgres。
// 需要同时设置 TEST_DATABASE_URL 和 TEST_NATS_URL（NATS 要开启 JetStream）。
func TestEndToEnd(t *testing.T) {
	pool := newTestDB(t)
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelAll := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelAll()

	suffix := time.Now().UnixNano()
	stream, prefix := fmt.Sprintf("E2E_%d", suffix), fmt.Sprintf("e2e%d", suffix)
	pub, err := outbox.NewNATSPublisher(ctx, nc, outbox.NATSOptions{Stream: stream, SubjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), stream) })

	cons, err := EnsureConsumer(ctx, js, stream, ConsumerConfig{
		Name: "e2e", Subjects: []string{prefix + ".user.created"}, AckWait: 5 * time.Second, MaxAckPending: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var failing atomic.Bool
	welcome := WelcomeHandler(discard)
	p := NewProcessor(pool, "e2e", map[string]Handler{"user.created": func(ctx context.Context, db DB, e Event) error {
		calls.Add(1)
		if failing.Load() {
			return Permanent(errors.New("simulated bug"))
		}
		return welcome(ctx, db, e)
	}}, ProcessorOptions{MaxDeliver: 3, HandlerTimeout: 5 * time.Second, AckWait: 5 * time.Second}, discard, nil)
	r := NewRunner(cons, p, RunnerOptions{BatchSize: 10, MaxWait: 500 * time.Millisecond}, discard)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	welcomeCount := func(uid string) int {
		return count(t, pool, `SELECT count(*) FROM welcome_notifications WHERE user_id = $1::uuid`, uid)
	}
	drained := func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	}

	// 1) 正常投递
	uid1 := "22222222-2222-4222-8222-222222222222"
	payload1 := []byte(`{"id":"` + uid1 + `","name":"A","email":"a@example.com"}`)
	if err := pub.Publish(ctx, outbox.Message{ID: 1001, AggregateID: uid1, Type: "user.created",
		Payload: payload1, Actor: "alice", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "first event processed", func() bool { return welcomeCount(uid1) == 1 })

	// 2) 去重窗口之外的重发：换一个 Nats-Msg-Id，JetStream 不会去重，要靠 processed_events
	dup := nats.NewMsg(prefix + ".user.created")
	dup.Data = payload1
	dup.Header.Set("Event-Id", "1001")
	dup.Header.Set("Event-Type", "user.created")
	if _, err := js.PublishMsg(ctx, dup, jetstream.WithMsgID("manual-duplicate")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "duplicate acknowledged", drained)
	if calls.Load() != 1 || welcomeCount(uid1) != 1 {
		t.Fatalf("after duplicate: handler calls=%d welcome rows=%d, want 1 and 1", calls.Load(), welcomeCount(uid1))
	}

	// 3) 处理失败进入死信，修复后重放
	failing.Store(true)
	uid2 := "33333333-3333-4333-8333-333333333333"
	if err := pub.Publish(ctx, outbox.Message{ID: 1002, AggregateID: uid2, Type: "user.created",
		Payload: []byte(`{"id":"` + uid2 + `","name":"B","email":"b@example.com"}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "dead letter written", func() bool {
		return count(t, pool, `SELECT count(*) FROM consumer_dead_letters WHERE consumer = 'e2e' AND event_id = 1002`) == 1
	})

	failing.Store(false)
	n, err := ReplayDeadLetters(ctx, pool, js, "e2e", 10)
	if err != nil || n != 1 {
		t.Fatalf("replayed=%d err=%v, want 1", n, err)
	}
	waitFor(t, 10*time.Second, "replayed event processed", func() bool { return welcomeCount(uid2) == 1 })
	if left := count(t, pool, `SELECT count(*) FROM consumer_dead_letters WHERE consumer = 'e2e'`); left != 0 {
		t.Fatalf("dead letters left = %d", left)
	}
}
