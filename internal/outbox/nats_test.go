package outbox

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// 集成测试：设置 TEST_NATS_URL 才会运行（需要开启 JetStream：nats-server -js）。
// 每次用独立的流名和主题，结束时删除。
func TestNATSPublisherDedupAndHeaders(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano()
	stream := fmt.Sprintf("TEST_%d", suffix)
	prefix := fmt.Sprintf("test%d", suffix)
	pub, err := NewNATSPublisher(ctx, nc, NATSOptions{Stream: stream, SubjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), stream) })

	m := Message{ID: 7, AggregateID: "agg-1", Type: "user.created", Payload: []byte(`{"id":"agg-1"}`),
		Actor: "alice", CreatedAt: time.Now()}
	for range 2 { // 第二次是模拟崩溃后的重发
		if err := pub.Publish(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("stream has %d messages, want 1 (duplicate should be dropped)", info.State.Msgs)
	}

	raw, err := s.GetMsg(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Subject != prefix+".user.created" || raw.Header.Get("Event-Type") != "user.created" ||
		raw.Header.Get("Actor") != "alice" || raw.Header.Get("Event-Id") != "7" ||
		raw.Header.Get("Nats-Msg-Id") != MessageID(7) {
		t.Fatalf("unexpected message: subject=%s headers=%v", raw.Subject, raw.Header)
	}

	if err := pub.Publish(ctx, Message{ID: 8, Type: "bad type"}); !IsPermanent(err) {
		t.Fatalf("invalid subject err = %v, want permanent", err)
	}
}
