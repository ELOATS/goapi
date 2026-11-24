// Command consumer 是一个示例事件消费者：订阅 user.created，为新用户记录一条欢迎通知。
// 可以同时运行多个副本（同一个 JetStream 持久消费者，消息会分给各副本）。
//
// 子命令：
//
//	consumer                         持续消费
//	consumer replay-dead-letters     把死信重新发布到原主题（修复问题之后用）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"example.com/goapi/internal/consumer"
	"example.com/goapi/internal/store"
	"example.com/goapi/internal/telemetry"
)

type config struct {
	Env         string
	LogLevel    slog.Level
	DatabaseURL string
	NATSURL     string
	MetricsAddr string

	Stream        string
	SubjectPrefix string
	StreamMaxAge  time.Duration

	Name           string
	AckWait        time.Duration
	MaxDeliver     int
	MaxAckPending  int
	BatchSize      int
	HandlerTimeout time.Duration
	DedupRetention time.Duration

	ServiceName      string
	TracingEnabled   bool
	TraceSampleRatio float64
}

func loadConfig() (config, error) {
	var errs []error
	c := config{
		Env:              env("APP_ENV", "dev"),
		DatabaseURL:      env("DATABASE_URL", ""),
		NATSURL:          env("NATS_URL", ""),
		MetricsAddr:      env("METRICS_ADDR", ":9090"),
		Stream:           env("NATS_STREAM", "GOAPI_EVENTS"),
		SubjectPrefix:    env("NATS_SUBJECT_PREFIX", "goapi.events"),
		StreamMaxAge:     dur("NATS_STREAM_MAX_AGE", 7*24*time.Hour, &errs),
		Name:             env("CONSUMER_NAME", "goapi-welcome"),
		AckWait:          dur("CONSUMER_ACK_WAIT", 30*time.Second, &errs),
		MaxDeliver:       num("CONSUMER_MAX_DELIVER", 10, &errs),
		MaxAckPending:    num("CONSUMER_MAX_ACK_PENDING", 1000, &errs),
		BatchSize:        num("CONSUMER_BATCH_SIZE", 10, &errs),
		HandlerTimeout:   dur("CONSUMER_HANDLER_TIMEOUT", 20*time.Second, &errs),
		DedupRetention:   dur("CONSUMER_DEDUP_RETENTION", 14*24*time.Hour, &errs),
		ServiceName:      env("OTEL_SERVICE_NAME", "goapi-consumer"),
		TracingEnabled:   env("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "" || env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "") != "",
		TraceSampleRatio: float("TRACE_SAMPLE_RATIO", 1.0, &errs),
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.NATSURL == "" {
		errs = append(errs, errors.New("NATS_URL is required"))
	}
	if c.AckWait < 5*time.Second {
		errs = append(errs, fmt.Errorf("CONSUMER_ACK_WAIT must be >= 5s, got %s", c.AckWait))
	}
	if c.MaxDeliver < 1 {
		errs = append(errs, errors.New("CONSUMER_MAX_DELIVER must be >= 1"))
	}
	if c.BatchSize < 1 || c.BatchSize > 1000 {
		errs = append(errs, fmt.Errorf("CONSUMER_BATCH_SIZE must be in [1, 1000], got %d", c.BatchSize))
	}
	if c.HandlerTimeout <= 0 {
		errs = append(errs, errors.New("CONSUMER_HANDLER_TIMEOUT must be > 0"))
	}
	// 去重记录比流里的消息先过期的话，之后的重投就会被当成新消息
	if c.StreamMaxAge > 0 && c.DedupRetention <= c.StreamMaxAge {
		errs = append(errs, fmt.Errorf("CONSUMER_DEDUP_RETENTION (%s) must be longer than NATS_STREAM_MAX_AGE (%s)",
			c.DedupRetention, c.StreamMaxAge))
	}
	if len(errs) > 0 {
		return config{}, fmt.Errorf("invalid config: %v", errs)
	}
	return c, nil
}

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "replay-dead-letters" {
		err = runReplay(os.Args[2:])
	} else {
		err = run()
	}
	if err != nil {
		slog.Error("consumer exited with error", "err", err)
		os.Exit(1)
	}
}

func newLogger(cfg config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.Env == "dev" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(telemetry.NewTraceLogHandler(h))
}

func connect(ctx context.Context, cfg config, logger *slog.Logger) (*pgxpool.Pool, *nats.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := store.NewPool(connectCtx, store.PoolConfig{
		URL: cfg.DatabaseURL, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: 30 * time.Minute, MaxConnIdleTime: 5 * time.Minute,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect postgres: %w", err)
	}
	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name(cfg.Name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { logger.Warn("nats disconnected", "err", err) }),
		nats.ReconnectHandler(func(c *nats.Conn) { logger.Info("nats reconnected", "server", c.ConnectedUrlRedacted()) }),
	)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("connect nats: %w", err)
	}
	return pool, nc, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled: cfg.TracingEnabled, ServiceName: cfg.ServiceName, Env: cfg.Env, SampleRatio: cfg.TraceSampleRatio,
	})
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flushCtx)
	}()

	pool, nc, err := connect(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	defer func() { _ = nc.Drain() }()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	cons, err := ensureConsumerWithRetry(ctx, js, cfg, logger)
	if err != nil {
		return err
	}

	reg := telemetry.NewRegistry()
	reg.MustRegister(consumer.NewLagCollector(cons, pool, cfg.Name))
	handlers := map[string]consumer.Handler{
		"user.created": consumer.WelcomeHandler(logger),
	}
	proc := consumer.NewProcessor(pool, cfg.Name, handlers, consumer.ProcessorOptions{
		MaxDeliver: cfg.MaxDeliver, HandlerTimeout: cfg.HandlerTimeout, AckWait: cfg.AckWait,
	}, logger, consumer.NewMetrics(reg))
	runner := consumer.NewRunner(cons, proc, consumer.RunnerOptions{BatchSize: cfg.BatchSize}, logger)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg, ErrorHandling: promhttp.ContinueOnError}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !runner.Healthy(2 * time.Minute) {
			http.Error(w, "consumer loop stalled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	metricsSrv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server", "err", err)
			stop()
		}
	}()

	go cleanupLoop(ctx, pool, cfg, logger)

	logger.Info("consumer started", "consumer", cfg.Name, "stream", cfg.Stream,
		"subjects", subjects(cfg), "max_deliver", cfg.MaxDeliver, "ack_wait", cfg.AckWait)
	runErr := runner.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	logger.Info("consumer stopped")
	return runErr
}

func subjects(cfg config) []string {
	// 要订阅更多事件：在这里加主题，并在 handlers 里注册对应的 Handler
	return []string{cfg.SubjectPrefix + ".user.created"}
}

// ensureConsumerWithRetry：流由 relay 创建，consumer 先启动时等一会儿，最多 2 分钟，之后退出让 Kubernetes 重启。
func ensureConsumerWithRetry(ctx context.Context, js jetstream.JetStream, cfg config, logger *slog.Logger) (jetstream.Consumer, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cons, err := consumer.EnsureConsumer(reqCtx, js, cfg.Stream, consumer.ConsumerConfig{
			Name: cfg.Name, Subjects: subjects(cfg), AckWait: cfg.AckWait, MaxAckPending: cfg.MaxAckPending,
		})
		cancel()
		if err == nil {
			return cons, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("create consumer %s on stream %s: %w", cfg.Name, cfg.Stream, err)
		}
		logger.Warn("cannot create consumer yet (has the relay created the stream?), retrying in 5s", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func cleanupLoop(ctx context.Context, pool *pgxpool.Pool, cfg config, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := consumer.CleanupProcessed(ctx, pool, cfg.Name, cfg.DedupRetention)
		if err != nil {
			logger.Warn("cleanup processed events failed", "err", err)
		} else if n > 0 {
			logger.Info("cleaned up processed events", "deleted", n, "retention", cfg.DedupRetention)
		}
	}
}

func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay-dead-letters", flag.ContinueOnError)
	limit := fs.Int("limit", 100, "最多重放多少条")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, nc, err := connect(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	defer func() { _ = nc.Drain() }()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	n, err := consumer.ReplayDeadLetters(ctx, pool, js, cfg.Name, *limit)
	logger.Info("dead letters replayed", "consumer", cfg.Name, "count", n)
	return err
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dur(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func num(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func float(key string, def float64, errs *[]error) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return f
}
