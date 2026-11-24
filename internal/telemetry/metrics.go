package telemetry

import (
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// NewRegistry 使用独立的 Registry，而不是全局 DefaultRegisterer，便于测试且不会混入第三方指标。
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// HTTPMetrics 是 RED 指标（Rate / Errors / Duration）。
// route 标签使用路由模板（/api/v1/users/{id}），绝不用原始路径，避免基数爆炸。
// 所有方法对 nil 接收者安全，测试里可以直接传 nil。
type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	f := promauto.With(reg)
	return &HTTPMetrics{
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by method, route and status code.",
		}, []string{"method", "route", "status"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method and route.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		inFlight: f.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
}

func (m *HTTPMetrics) Begin() {
	if m != nil {
		m.inFlight.Inc()
	}
}

func (m *HTTPMetrics) End(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.inFlight.Dec()
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(d.Seconds())
}

// poolCollector 在每次抓取时读取 pgxpool.Stat()，无需后台 goroutine。
type poolCollector struct {
	pool *pgxpool.Pool

	acquired, idle, total, max, constructing *prometheus.Desc
	acquireCount, acquireWait, emptyAcquire, canceledAcquire *prometheus.Desc
}

// NewPoolCollector 导出连接池指标。最值得告警的是 db_pool_empty_acquire_total 的增长率：
// 它表示请求拿连接时池子已空、必须排队，通常意味着 DB_MAX_CONNS 太小或有慢查询占着连接。
func NewPoolCollector(pool *pgxpool.Pool) prometheus.Collector {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return &poolCollector{
		pool:            pool,
		acquired:        d("db_pool_acquired_conns", "Connections currently checked out."),
		idle:            d("db_pool_idle_conns", "Idle connections in the pool."),
		total:           d("db_pool_total_conns", "Total connections in the pool."),
		max:             d("db_pool_max_conns", "Configured maximum pool size."),
		constructing:    d("db_pool_constructing_conns", "Connections currently being established."),
		acquireCount:    d("db_pool_acquire_total", "Successful connection acquisitions."),
		acquireWait:     d("db_pool_acquire_wait_seconds_total", "Total time spent acquiring connections."),
		emptyAcquire:    d("db_pool_empty_acquire_total", "Acquisitions that had to wait because the pool was empty."),
		canceledAcquire: d("db_pool_canceled_acquire_total", "Acquisitions canceled by context."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.constructing,
		c.acquireCount, c.acquireWait, c.emptyAcquire, c.canceledAcquire} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	ct := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v) }

	g(c.acquired, float64(s.AcquiredConns()))
	g(c.idle, float64(s.IdleConns()))
	g(c.total, float64(s.TotalConns()))
	g(c.max, float64(s.MaxConns()))
	g(c.constructing, float64(s.ConstructingConns()))
	ct(c.acquireCount, float64(s.AcquireCount()))
	ct(c.acquireWait, s.AcquireDuration().Seconds())
	ct(c.emptyAcquire, float64(s.EmptyAcquireCount()))
	ct(c.canceledAcquire, float64(s.CanceledAcquireCount()))
}
