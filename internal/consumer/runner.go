package consumer

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ConsumerConfig 描述一个 JetStream 持久（durable）拉取消费者。
// 同名的多个副本共享同一个消费者，JetStream 会把消息分给它们，互不重复（重投除外）。
type ConsumerConfig struct {
	Name          string   // 持久消费者名，也是去重记录里的 consumer
	Subjects      []string // 只接收这些主题（需要 NATS 2.10+）
	AckWait       time.Duration
	MaxAckPending int // 所有副本合计最多有多少条在处理中
}

// EnsureConsumer 创建或更新持久消费者。流由 relay 创建，所以要在 relay 至少启动过一次之后调用。
func EnsureConsumer(ctx context.Context, js jetstream.JetStream, stream string, c ConsumerConfig) (jetstream.Consumer, error) {
	return js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:        c.Name,
		FilterSubjects: c.Subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        c.AckWait,
		// 不让 JetStream 自己放弃消息：重试次数由处理器控制，用完写入死信再 Term。
		// 否则处理器崩溃在最后一次尝试时，消息会被 JetStream 静默丢弃。
		MaxDeliver:    -1,
		MaxAckPending: c.MaxAckPending,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
}

// Fetcher 是 jetstream.Consumer 的拉取方法。
type Fetcher interface {
	Fetch(batch int, opts ...jetstream.FetchOpt) (jetstream.MessageBatch, error)
}

type RunnerOptions struct {
	BatchSize int           // 每次拉取多少条，默认 10
	MaxWait   time.Duration // 没有消息时一次拉取最多等多久，默认 5s
}

type Runner struct {
	cons     Fetcher
	p        *Processor
	opt      RunnerOptions
	log      *slog.Logger
	lastLoop atomic.Int64
}

func NewRunner(cons Fetcher, p *Processor, opt RunnerOptions, logger *slog.Logger) *Runner {
	if opt.BatchSize <= 0 {
		opt.BatchSize = 10
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 5 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Runner{cons: cons, p: p, opt: opt, log: logger}
	r.touch()
	return r
}

func (r *Runner) touch() { r.lastLoop.Store(time.Now().UnixNano()) }

// Healthy 主循环在 maxAge 内运行过就返回 true，供存活探针使用。
func (r *Runner) Healthy(maxAge time.Duration) bool {
	return time.Since(time.Unix(0, r.lastLoop.Load())) <= maxAge
}

// Run 循环拉取并处理消息，直到 ctx 被取消。手上这条会处理完；同一批里还没开始的立即 Nak，
// 让其它副本马上接手，而不是等 AckWait 超时。
func (r *Runner) Run(ctx context.Context) error {
	var backoff time.Duration
	for {
		r.touch()
		if ctx.Err() != nil {
			return nil
		}

		batch, err := r.cons.Fetch(r.opt.BatchSize, jetstream.FetchMaxWait(r.opt.MaxWait))
		if err != nil {
			backoff = nextBackoff(backoff)
			r.log.WarnContext(ctx, "fetch failed", "err", err, "retry_in", backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			continue
		}
		backoff = 0

		for msg := range batch.Messages() {
			if ctx.Err() != nil {
				_ = msg.Nak()
				continue
			}
			r.p.Process(ctx, msg)
			r.touch()
		}
		if err := batch.Error(); err != nil && !errors.Is(err, nats.ErrTimeout) && ctx.Err() == nil {
			r.log.DebugContext(ctx, "fetch batch ended with error", "err", err)
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	switch {
	case cur <= 0:
		return time.Second
	case cur >= 30*time.Second:
		return 30 * time.Second
	default:
		return cur * 2
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
