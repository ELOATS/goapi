package outbox

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics 记录投递情况。所有方法对 nil 接收者安全。
type Metrics struct {
	published prometheus.Counter
	failures  *prometheus.CounterVec
	delay     prometheus.Histogram
}

// NewMetrics 注册投递指标；pool 不为 nil 时，还会在每次抓取时查询积压情况。
func NewMetrics(reg prometheus.Registerer, pool *pgxpool.Pool) *Metrics {
	f := promauto.With(reg)
	m := &Metrics{
		published: f.NewCounter(prometheus.CounterOpts{
			Name: "outbox_events_published_total",
			Help: "Outbox events published to the message broker.",
		}),
		failures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_publish_failures_total",
			Help: "Outbox publish failures by kind (transient, permanent).",
		}, []string{"kind"}),
		delay: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "outbox_event_delay_seconds",
			Help:    "Time from event creation to successful publish.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}),
	}
	if pool != nil {
		reg.MustRegister(newBacklogCollector(pool))
	}
	return m
}

func (m *Metrics) observePublished(createdAt time.Time) {
	if m == nil {
		return
	}
	m.published.Inc()
	m.delay.Observe(time.Since(createdAt).Seconds())
}

func (m *Metrics) observeFailure(kind string) {
	if m != nil {
		m.failures.WithLabelValues(kind).Inc()
	}
}

// backlogCollector 在抓取时查一次数据库。多个 relay 副本会报出相同的值，告警和面板里用 max()。
// 查询失败时不输出这几个指标（而不是让整个 /metrics 失败），以免数据库故障时误报 relay 宕机。
type backlogCollector struct {
	pool                *pgxpool.Pool
	pending, oldest, dead *prometheus.Desc
}

func newBacklogCollector(pool *pgxpool.Pool) prometheus.Collector {
	return &backlogCollector{
		pool:    pool,
		pending: prometheus.NewDesc("outbox_pending_events", "Events waiting to be published.", nil, nil),
		oldest:  prometheus.NewDesc("outbox_oldest_pending_age_seconds", "Age of the oldest event waiting to be published (0 if none).", nil, nil),
		dead:    prometheus.NewDesc("outbox_dead_events", "Events that cannot be delivered and are no longer retried.", nil, nil),
	}
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.oldest
	ch <- c.dead
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var pending, dead int64
	var oldest float64
	err := c.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE dead_at IS NULL),
			coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE dead_at IS NULL)), 0)::float8,
			count(*) FILTER (WHERE dead_at IS NOT NULL)
		FROM outbox_events
		WHERE published_at IS NULL`).Scan(&pending, &oldest, &dead)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, oldest)
	ch <- prometheus.MustNewConstMetric(c.dead, prometheus.GaugeValue, float64(dead))
}
