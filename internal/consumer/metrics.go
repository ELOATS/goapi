package consumer

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics 记录处理情况。所有方法对 nil 接收者安全。
type Metrics struct {
	messages *prometheus.CounterVec
	duration *prometheus.HistogramVec
	delay    prometheus.Histogram
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		messages: f.NewCounterVec(prometheus.CounterOpts{
			Name: "consumer_messages_total",
			Help: "Messages handled by event type and outcome (processed, duplicate, retry, dead, skipped, malformed).",
		}, []string{"event_type", "result"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "consumer_handle_duration_seconds",
			Help:    "Time spent handling one message, including the database transaction.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20},
		}, []string{"event_type"}),
		delay: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "consumer_event_delay_seconds",
			Help:    "Time from event creation (Occurred-At) to successful processing.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 900},
		}),
	}
}

func (m *Metrics) observe(eventType string, o Outcome) {
	if m != nil {
		m.messages.WithLabelValues(eventType, string(o)).Inc()
	}
}

func (m *Metrics) observeDuration(eventType string, d time.Duration) {
	if m != nil {
		m.duration.WithLabelValues(eventType).Observe(d.Seconds())
	}
}

func (m *Metrics) observeDelay(d time.Duration) {
	if m != nil {
		m.delay.Observe(d.Seconds())
	}
}

// ConsumerInfoer 是 jetstream.Consumer 的 Info 方法。
type ConsumerInfoer interface {
	Info(ctx context.Context) (*jetstream.ConsumerInfo, error)
}

// NewLagCollector 在抓取时查询 JetStream 的积压和数据库里的死信数。多个副本报出的值相同，告警里用 max()。
// 查询失败时不输出对应指标，而不是让整个 /metrics 失败。
func NewLagCollector(cons ConsumerInfoer, pool *pgxpool.Pool, consumerName string) prometheus.Collector {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return &lagCollector{
		cons:        cons,
		pool:        pool,
		name:        consumerName,
		pending:     d("consumer_pending_messages", "Messages in the stream not yet delivered to this consumer."),
		ackPending:  d("consumer_ack_pending_messages", "Messages delivered but not yet acknowledged."),
		redelivered: d("consumer_redelivered_messages", "Messages currently pending redelivery."),
		dead:        d("consumer_dead_letters", "Messages in the dead letter table for this consumer."),
	}
}

type lagCollector struct {
	cons                                   ConsumerInfoer
	pool                                   *pgxpool.Pool
	name                                   string
	pending, ackPending, redelivered, dead *prometheus.Desc
}

func (c *lagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.ackPending
	ch <- c.redelivered
	ch <- c.dead
}

func (c *lagCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }

	if info, err := c.cons.Info(ctx); err == nil && info != nil {
		g(c.pending, float64(info.NumPending))
		g(c.ackPending, float64(info.NumAckPending))
		g(c.redelivered, float64(info.NumRedelivered))
	}
	var dead int64
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM consumer_dead_letters WHERE consumer = $1`, c.name).Scan(&dead); err == nil {
		g(c.dead, float64(dead))
	}
}
