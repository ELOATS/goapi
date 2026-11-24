package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"ELOATS/goapi/internal/telemetry"
)

// DB 是处理器传给 Handler 的事务，pgx.Tx 满足它。
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Handler 处理一个事件。所有数据库写入都要用传入的 db：它和去重记录在同一个事务里，
// 返回错误时整个事务回滚，消息稍后重投。不要在这里做无法回滚的外部调用（例如直接发邮件），
// 而是像示例一样先写一条待办记录，由别的进程执行；或者确保那个调用本身是幂等的。
type Handler func(ctx context.Context, db DB, e Event) error

// Message 是处理器用到的 jetstream.Msg 方法子集，测试里可以用假的实现。
type Message interface {
	Metadata() (*jetstream.MsgMetadata, error)
	Data() []byte
	Headers() nats.Header
	Subject() string
	Ack() error
	Nak() error
	NakWithDelay(delay time.Duration) error
	InProgress() error
	Term() error
}

// Outcome 是一条消息的处理结果，也是指标里的 result 标签。
type Outcome string

const (
	OutcomeProcessed Outcome = "processed" // 处理成功
	OutcomeDuplicate Outcome = "duplicate" // 之前处理过，直接确认
	OutcomeRetry     Outcome = "retry"     // 暂时性失败，稍后重投
	OutcomeDead      Outcome = "dead"      // 进入死信，不再重投
	OutcomeSkipped   Outcome = "skipped"   // 没有对应的 Handler，直接确认
	OutcomeMalformed Outcome = "malformed" // 缺少必要的消息头，终止
)

type ProcessorOptions struct {
	MaxDeliver     int           // 最多尝试几次，用完进死信，默认 10
	HandlerTimeout time.Duration // 单次处理（含事务）超时，默认 20s
	AckWait        time.Duration // 与 JetStream 消费者的 AckWait 一致，用来计算心跳间隔，默认 30s
	BackoffBase    time.Duration // 第一次重试的间隔，默认 1s
	BackoffMax     time.Duration // 最大重试间隔，默认 5m
}

func (o *ProcessorOptions) setDefaults() {
	if o.MaxDeliver <= 0 {
		o.MaxDeliver = 10
	}
	if o.HandlerTimeout <= 0 {
		o.HandlerTimeout = 20 * time.Second
	}
	if o.AckWait <= 0 {
		o.AckWait = 30 * time.Second
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 5 * time.Minute
	}
}

type Processor struct {
	pool       *pgxpool.Pool
	name       string
	handlers   map[string]Handler
	opt        ProcessorOptions
	maxDeliver uint64 // 由 opt.MaxDeliver 换算
	log        *slog.Logger
	metrics    *Metrics
	tracer     trace.Tracer
}

// NewProcessor：name 是消费者名，用于去重记录和死信；handlers 按事件类型注册。
func NewProcessor(pool *pgxpool.Pool, name string, handlers map[string]Handler, opt ProcessorOptions, logger *slog.Logger, m *Metrics) *Processor {
	opt.setDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{
		pool:       pool,
		name:       name,
		handlers:   handlers,
		opt:        opt,
		maxDeliver: uint64(opt.MaxDeliver), //nolint:gosec // setDefaults 保证 MaxDeliver >= 1
		log:        logger.With("consumer", name),
		metrics:    m,
		tracer:     otel.Tracer("example.com/goapi/internal/consumer"),
	}
}

// Process 处理一条消息，并负责确认（Ack / NakWithDelay / Term）。
func (p *Processor) Process(ctx context.Context, msg Message) Outcome {
	delivered := uint64(1)
	if md, err := msg.Metadata(); err == nil && md != nil && md.NumDelivered > 0 {
		delivered = md.NumDelivered
	}

	e, err := ParseEvent(msg.Subject(), msg.Headers(), msg.Data())
	if err != nil {
		p.log.ErrorContext(ctx, "malformed message, terminating", "subject", msg.Subject(), "err", err)
		p.settle(ctx, msg.Term, "term")
		p.metrics.observe("unknown", OutcomeMalformed)
		return OutcomeMalformed
	}
	e.Delivery = delivered

	h, ok := p.handlers[e.Type]
	if !ok {
		p.settle(ctx, msg.Ack, "ack")
		p.metrics.observe(e.Type, OutcomeSkipped)
		return OutcomeSkipped
	}

	// 接上生产方的 trace：API 请求 → relay 发布 → 这里的消费
	parent := otel.GetTextMapPropagator().Extract(ctx, telemetry.NATSHeaderCarrier(msg.Headers()))
	sctx, span := p.tracer.Start(parent, "consume "+e.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.operation.type", "process"),
			attribute.String("messaging.destination.name", e.Subject),
			attribute.String("messaging.consumer.group.name", p.name),
			attribute.Int64("event.id", e.ID),
			attribute.Int64("messaging.delivery_count", int64(delivered)),
		))
	defer span.End()

	stop := p.heartbeat(msg)
	start := time.Now()
	duplicate, err := p.handle(sctx, h, e)
	stop()
	p.metrics.observeDuration(e.Type, time.Since(start))

	switch {
	case err == nil && duplicate:
		p.settle(sctx, msg.Ack, "ack")
		p.metrics.observe(e.Type, OutcomeDuplicate)
		return OutcomeDuplicate

	case err == nil:
		// 事务已提交。确认失败也没关系：消息会被重投，但去重记录会挡住
		p.settle(sctx, msg.Ack, "ack")
		p.metrics.observe(e.Type, OutcomeProcessed)
		if !e.OccurredAt.IsZero() {
			p.metrics.observeDelay(time.Since(e.OccurredAt))
		}
		return OutcomeProcessed

	case IsPermanent(err) || delivered >= p.maxDeliver:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if dlErr := p.deadLetter(sctx, e, msg, err); dlErr != nil {
			// 死信写不进去（数据库故障）时不能终止消息，否则就丢了：稍后再试
			delay := Backoff(delivered, p.opt.BackoffBase, p.opt.BackoffMax)
			p.log.ErrorContext(sctx, "cannot write dead letter, will retry", "event_id", e.ID, "err", dlErr, "retry_in", delay)
			p.settle(sctx, func() error { return msg.NakWithDelay(delay) }, "nak")
			p.metrics.observe(e.Type, OutcomeRetry)
			return OutcomeRetry
		}
		p.log.ErrorContext(sctx, "event moved to dead letter", "event_id", e.ID, "event_type", e.Type,
			"attempts", delivered, "permanent", IsPermanent(err), "err", err)
		p.settle(sctx, msg.Term, "term")
		p.metrics.observe(e.Type, OutcomeDead)
		return OutcomeDead

	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		delay := Backoff(delivered, p.opt.BackoffBase, p.opt.BackoffMax)
		p.log.WarnContext(sctx, "event handling failed, will retry", "event_id", e.ID, "event_type", e.Type,
			"attempt", delivered, "max_deliver", p.opt.MaxDeliver, "retry_in", delay, "err", err)
		p.settle(sctx, func() error { return msg.NakWithDelay(delay) }, "nak")
		p.metrics.observe(e.Type, OutcomeRetry)
		return OutcomeRetry
	}
}

// handle 在一个事务里先写去重记录再执行 Handler。去重记录已存在（主键冲突）说明处理过，返回 duplicate。
// 两个副本同时处理同一条重投消息时，后插入的一方会等前一方提交，然后得到 0 行，判定为重复。
func (p *Processor) handle(ctx context.Context, h Handler, e Event) (duplicate bool, err error) {
	// 不跟随调用方取消：关闭时让手上这条处理完，而不是做到一半回滚
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opt.HandlerTimeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx,
		`INSERT INTO processed_events (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		p.name, e.ID)
	if err != nil {
		return false, fmt.Errorf("record processed event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return true, nil
	}
	if err := h(ctx, tx, e); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return false, nil
}

func (p *Processor) deadLetter(ctx context.Context, e Event, msg Message, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	headers, err := json.Marshal(msg.Headers())
	if err != nil {
		headers = []byte("{}")
	}
	payload := msg.Data()
	if payload == nil {
		payload = []byte{}
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO consumer_dead_letters (consumer, event_id, event_type, subject, headers, payload, error, attempts)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
		ON CONFLICT (consumer, event_id) DO UPDATE
		SET error = EXCLUDED.error, attempts = EXCLUDED.attempts, created_at = now()`,
		p.name, e.ID, e.Type, e.Subject, string(headers), payload, truncate(cause.Error(), 2000), toInt64(e.Delivery))
	return err
}

// heartbeat 在处理期间定期告诉 JetStream "还在处理"，避免处理时间超过 AckWait 被重投给别的副本。
func (p *Processor) heartbeat(msg Message) (stop func()) {
	interval := p.opt.AckWait / 3
	if interval < time.Second {
		interval = time.Second
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := msg.InProgress(); err != nil {
					p.log.Debug("in-progress heartbeat failed", "err", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func (p *Processor) settle(ctx context.Context, f func() error, action string) {
	if err := f(); err != nil {
		p.log.WarnContext(ctx, "failed to settle message", "action", action, "err", err)
	}
}

// CleanupProcessed 分批删除超过保留期的去重记录。保留期必须长于消息可能被重投的时间
// （JetStream 流的 MaxAge），否则过期之后重投的消息会被当成新消息再处理一次。
func CleanupProcessed(ctx context.Context, pool *pgxpool.Pool, consumerName string, retention time.Duration) (int64, error) {
	const batch = 1000
	var total int64
	for {
		tag, err := pool.Exec(ctx, `DELETE FROM processed_events WHERE ctid IN (
			SELECT ctid FROM processed_events
			WHERE consumer = $1 AND processed_at < now() - make_interval(secs => $2)
			LIMIT $3)`, consumerName, retention.Seconds(), batch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < batch || ctx.Err() != nil {
			return total, nil
		}
	}
}

func toInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v) //nolint:gosec // 上面已经截断
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
