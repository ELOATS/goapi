// Package outbox 把 outbox_events 表里的事件可靠地投递到消息系统（transactional outbox 的 relay 一侧）。
//
// 投递语义是"至少一次"：发布成功但标记已投递之前进程崩溃，事件会被再发一次。
// NATS 发布时带 Nats-Msg-Id，服务端在去重窗口内会丢掉重复消息；窗口之外由消费方按事件 ID 幂等处理。
//
// 多个 relay 实例可以同时运行：每批用 FOR UPDATE SKIP LOCKED 认领，互不重复。
// 代价是不保证全局顺序；同一聚合的事件需要严格有序时，消费方应按 Event-Id 或业务版本号处理。
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Message 是一条待投递的事件。
type Message struct {
	ID          int64
	AggregateID string
	Type        string
	Payload     []byte // JSON
	Actor       string
	CreatedAt   time.Time
}

// Publisher 把事件发到消息系统。成功返回 nil 时，消息必须已被持久化确认（例如 JetStream 的 PubAck）。
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent 标记"重试也不会成功"的错误（例如消息过大、主题非法）：事件会直接进入死信，不再重试。
// 其它错误都视为暂时性的（网络、消息系统不可用），会一直重试。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type Options struct {
	BatchSize      int           // 每批最多认领多少条，默认 100
	PollInterval   time.Duration // 没有通知时的轮询间隔，默认 1s
	MaxBackoff     time.Duration // 投递失败后的最大退避，默认 30s
	PublishTimeout time.Duration // 单条发布超时，默认 5s
	BatchTimeout   time.Duration // 整批（事务）超时，默认 30s；超时则整批回滚、稍后重发
	Listen         bool          // 用 LISTEN/NOTIFY 及时唤醒；PgBouncer 事务池模式下要关掉
	Retention      time.Duration // 已投递事件保留多久，0 表示不清理
	CleanupEvery   time.Duration // 清理间隔，默认 1h
}

func (o *Options) setDefaults() {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = 5 * time.Second
	}
	if o.BatchTimeout <= 0 {
		o.BatchTimeout = 30 * time.Second
	}
	if o.CleanupEvery <= 0 {
		o.CleanupEvery = time.Hour
	}
}

const (
	notifyChannel = "outbox_events"
	maxErrorLen   = 500
	cleanupBatch  = 1000
)

type Relay struct {
	pool     *pgxpool.Pool
	pub      Publisher
	opt      Options
	log      *slog.Logger
	metrics  *Metrics
	tracer   trace.Tracer
	lastLoop atomic.Int64 // 最近一次主循环的时间，供存活探针判断
}

func New(pool *pgxpool.Pool, pub Publisher, opt Options, logger *slog.Logger, metrics *Metrics) *Relay {
	opt.setDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	r := &Relay{
		pool:    pool,
		pub:     pub,
		opt:     opt,
		log:     logger,
		metrics: metrics,
		tracer:  otel.Tracer("example.com/goapi/internal/outbox"),
	}
	r.touch()
	return r
}

func (r *Relay) touch() { r.lastLoop.Store(time.Now().UnixNano()) }

// Healthy 主循环在 maxAge 内运行过就返回 true；卡死（例如死锁）时存活探针会失败并重启进程。
func (r *Relay) Healthy(maxAge time.Duration) bool {
	return time.Since(time.Unix(0, r.lastLoop.Load())) <= maxAge
}

// Run 持续投递，直到 ctx 被取消。正在处理的批次会处理完再退出。
func (r *Relay) Run(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	if r.opt.Listen {
		go r.listen(ctx, wake)
	}
	if r.opt.Retention > 0 {
		go r.cleanupLoop(ctx)
	}

	var backoff time.Duration
	for {
		r.touch()
		n, err := r.ProcessBatch(ctx)
		if err != nil {
			backoff = nextBackoff(backoff, r.opt.PollInterval, r.opt.MaxBackoff)
			r.log.WarnContext(ctx, "outbox batch failed", "err", err, "retry_in", backoff)
		} else {
			backoff = 0
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && n == r.opt.BatchSize {
			continue // 还有积压，马上处理下一批
		}

		wait, wakeCh := r.opt.PollInterval, (<-chan struct{})(wake)
		if backoff > 0 {
			wait, wakeCh = backoff, nil // 退避期间不被通知提前唤醒
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		case <-wakeCh:
			t.Stop()
		}
	}
}

// nextBackoff 从 initial 开始每次翻倍，最大 maxDelay。
func nextBackoff(cur, initial, maxDelay time.Duration) time.Duration {
	if cur <= 0 {
		return initial
	}
	return min(cur*2, maxDelay)
}

const selectPending = `
SELECT id, aggregate_id::text, event_type, payload, coalesce(actor, ''), created_at, trace_context
FROM outbox_events
WHERE published_at IS NULL AND dead_at IS NULL
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED`

type pending struct {
	Message
	traceContext []byte
}

// ProcessBatch 认领一批事件并逐条发布，返回认领到的条数。
// 遇到暂时性错误会停在那一条（后面的留给下一批），已成功的照常标记，并返回错误让调用方退避。
func (r *Relay) ProcessBatch(ctx context.Context) (int, error) {
	// 不跟随调用方取消：关闭时让当前批次正常收尾，避免刚发出去的消息被回滚后重发
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opt.BatchTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, selectPending, r.opt.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("select pending: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.ID, &p.AggregateID, &p.Type, &p.Payload, &p.Actor, &p.CreatedAt, &p.traceContext)
		return p, err
	})
	if err != nil {
		return 0, fmt.Errorf("scan pending: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	var published []int64
	var publishErr error
	for _, p := range batch {
		err := r.publish(ctx, p)
		if err == nil {
			published = append(published, p.ID)
			r.metrics.observePublished(p.CreatedAt)
			continue
		}
		if IsPermanent(err) {
			r.metrics.observeFailure("permanent")
			r.log.ErrorContext(ctx, "outbox event cannot be delivered, moved to dead letter",
				"event_id", p.ID, "event_type", p.Type, "err", err)
			if _, e := tx.Exec(ctx, `UPDATE outbox_events
				SET attempts = attempts + 1, last_error = $2, dead_at = now() WHERE id = $1`,
				p.ID, truncate(err.Error())); e != nil {
				return len(batch), fmt.Errorf("mark dead: %w", e)
			}
			continue
		}
		// 暂时性错误：大概率是消息系统不可用，后面的也会失败，停在这里
		r.metrics.observeFailure("transient")
		if _, e := tx.Exec(ctx, `UPDATE outbox_events
			SET attempts = attempts + 1, last_error = $2 WHERE id = $1`,
			p.ID, truncate(err.Error())); e != nil {
			return len(batch), fmt.Errorf("record attempt: %w", e)
		}
		publishErr = fmt.Errorf("publish event %d: %w", p.ID, err)
		break
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE outbox_events
			SET published_at = now(), last_error = NULL WHERE id = ANY($1)`, published); err != nil {
			return len(batch), fmt.Errorf("mark published: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return len(batch), fmt.Errorf("commit: %w", err)
	}
	return len(batch), publishErr
}

// publish 用写入时保存的链路上下文作为父节点，创建 producer span，让投递出现在原请求的 trace 里。
func (r *Relay) publish(ctx context.Context, p pending) error {
	parent := ctx
	if len(p.traceContext) > 0 {
		carrier := propagation.MapCarrier{}
		if json.Unmarshal(p.traceContext, &carrier) == nil {
			parent = otel.GetTextMapPropagator().Extract(ctx, carrier)
		}
	}
	sctx, span := r.tracer.Start(parent, "outbox publish "+p.Type,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.operation.type", "send"),
			attribute.Int64("outbox.event_id", p.ID),
			attribute.String("outbox.event_type", p.Type),
		))
	defer span.End()

	pctx, cancel := context.WithTimeout(sctx, r.opt.PublishTimeout)
	defer cancel()
	err := r.pub.Publish(pctx, p.Message)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// listen 等待插入触发器发出的通知，及时唤醒主循环；连接断了 5 秒后重连，期间轮询照常进行。
func (r *Relay) listen(ctx context.Context, wake chan<- struct{}) {
	for {
		err := r.listenOnce(ctx, wake)
		if ctx.Err() != nil {
			return
		}
		r.log.WarnContext(ctx, "outbox LISTEN connection lost, retrying in 5s (polling continues)", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *Relay) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	pc, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// 从连接池里拿走这条连接：LISTEN 状态不能还给连接池给别人用
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default: // 已经有一个待处理的唤醒
		}
	}
}

func (r *Relay) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(r.opt.CleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := r.Cleanup(ctx)
		if err != nil {
			r.log.WarnContext(ctx, "outbox cleanup failed", "err", err)
			continue
		}
		if n > 0 {
			r.log.InfoContext(ctx, "outbox cleanup", "deleted", n, "retention", r.opt.Retention)
		}
	}
}

// Cleanup 分批删除超过保留期的已投递事件。死信不会被删除，处理完再手动清理。
func (r *Relay) Cleanup(ctx context.Context) (int64, error) {
	var total int64
	for {
		tag, err := r.pool.Exec(ctx, `DELETE FROM outbox_events WHERE id IN (
			SELECT id FROM outbox_events
			WHERE published_at < now() - make_interval(secs => $1)
			ORDER BY published_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)`, r.opt.Retention.Seconds(), cleanupBatch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < cleanupBatch || ctx.Err() != nil {
			return total, nil
		}
	}
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxErrorLen {
		return string(r[:maxErrorLen]) + "…"
	}
	return s
}
