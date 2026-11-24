package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"

	"ELOATS/goapi/internal/telemetry"
)

type NATSOptions struct {
	Stream        string        // JetStream 流名，例如 GOAPI_EVENTS
	SubjectPrefix string        // 主题前缀，事件发到 <前缀>.<事件类型>，例如 goapi.events.user.created
	Replicas      int           // 流的副本数，单节点 NATS 只能是 1
	MaxAge        time.Duration // 消息在流里保留多久，0 表示不按时间删除
	Duplicates    time.Duration // 去重窗口：窗口内相同 Nats-Msg-Id 的消息只保留一条，默认 2m
}

// NATSPublisher 发布到 NATS JetStream，等服务端确认（PubAck）后才算成功。
type NATSPublisher struct {
	js     jetstream.JetStream
	prefix string
}

// NewNATSPublisher 启动时创建或更新流，确保主题有流接收（否则发布会因无人响应而失败）。
func NewNATSPublisher(ctx context.Context, nc *nats.Conn, o NATSOptions) (*NATSPublisher, error) {
	if o.Stream == "" || !validSubject(o.SubjectPrefix) {
		return nil, fmt.Errorf("outbox: invalid NATS stream %q or subject prefix %q", o.Stream, o.SubjectPrefix)
	}
	if o.Replicas <= 0 {
		o.Replicas = 1
	}
	if o.Duplicates <= 0 {
		o.Duplicates = 2 * time.Minute
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       o.Stream,
		Subjects:   []string{o.SubjectPrefix + ".>"},
		Storage:    jetstream.FileStorage,
		Replicas:   o.Replicas,
		MaxAge:     o.MaxAge,
		Duplicates: o.Duplicates,
	}); err != nil {
		return nil, fmt.Errorf("create or update stream %s: %w", o.Stream, err)
	}
	return &NATSPublisher{js: js, prefix: o.SubjectPrefix}, nil
}

// MessageID 是 JetStream 去重用的消息 ID，同一个事件重发时保持不变。
func MessageID(eventID int64) string { return "goapi-outbox-" + strconv.FormatInt(eventID, 10) }

func (p *NATSPublisher) Publish(ctx context.Context, m Message) error {
	if !validSubject(m.Type) {
		return Permanent(fmt.Errorf("event type %q is not a valid NATS subject token", m.Type))
	}
	msg := nats.NewMsg(p.prefix + "." + m.Type)
	msg.Data = m.Payload
	msg.Header.Set("Event-Id", strconv.FormatInt(m.ID, 10))
	msg.Header.Set("Event-Type", m.Type)
	msg.Header.Set("Aggregate-Id", m.AggregateID)
	msg.Header.Set("Occurred-At", m.CreatedAt.UTC().Format(time.RFC3339Nano))
	if m.Actor != "" {
		msg.Header.Set("Actor", m.Actor)
	}
	// 按 W3C 规范写小写的 traceparent；消费方用 telemetry.NATSHeaderCarrier(msg.Header) 提取
	otel.GetTextMapPropagator().Inject(ctx, telemetry.NATSHeaderCarrier(msg.Header))

	_, err := p.js.PublishMsg(ctx, msg, jetstream.WithMsgID(MessageID(m.ID)))
	if errors.Is(err, nats.ErrMaxPayload) {
		return Permanent(err)
	}
	return err
}

// validSubject：用点分隔的非空片段，不含空白和通配符。
func validSubject(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\r\n*>") {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
	}
	return true
}

// LogPublisher 只把事件打印到日志，用于本地没有 NATS 的情况。事件会被标记为已投递。
type LogPublisher struct {
	Logger *slog.Logger
}

func (p LogPublisher) Publish(ctx context.Context, m Message) error {
	p.Logger.InfoContext(ctx, "dry-run: event published",
		"event_id", m.ID, "event_type", m.Type, "aggregate_id", m.AggregateID,
		"actor", m.Actor, "payload", string(m.Payload))
	return nil
}
