// Command relay 把 outbox_events 里的事件投递到 NATS JetStream。
// 可以同时运行多个副本；没有设置 NATS_URL 时只把事件打印到日志（仅限本地）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"example.com/goapi/internal/outbox"
	"example.com/goapi/internal/store"
	"example.com/goapi/internal/telemetry"
)

type config struct {
	Env         string
	LogLevel    slog.Level
	DatabaseURL string
	MetricsAddr string

	NATSURL        string
	NATSStream     string
	NATSPrefix     string
	NATSReplicas   int
	NATSMaxAge     time.Duration
	NATSDuplicates time.Duration

	Relay outbox.Options

	ServiceName      string
	TracingEnabled   bool
	TraceSampleRatio float64
}

func loadConfig() (config, error) {
	var errs []error
	c := config{
		Env:            env("APP_ENV", "dev"),
		DatabaseURL:    env("DATABASE_URL", ""),
		MetricsAddr:    env("METRICS_ADDR", ":9090"),
		NATSURL:        env("NATS_URL", ""),
		NATSStream:     env("NATS_STREAM", "GOAPI_EVENTS"),
		NATSPrefix:     env("NATS_SUBJECT_PREFIX", "goapi.events"),
		NATSReplicas:   num("NATS_STREAM_REPLICAS", 1, &errs),
		NATSMaxAge:     dur("NATS_STREAM_MAX_AGE", 7*24*time.Hour, &errs),
		NATSDuplicates: dur("NATS_DUPLICATE_WINDOW", 2*time.Minute, &errs),
		Relay: outbox.Options{
			BatchSize:      num("RELAY_BATCH_SIZE", 100, &errs),
			PollInterval:   dur("RELAY_POLL_INTERVAL", time.Second, &errs),
			MaxBackoff:     dur("RELAY_MAX_BACKOFF", 30*time.Second, &errs),
			PublishTimeout: dur("RELAY_PUBLISH_TIMEOUT", 5*time.Second, &errs),
			BatchTimeout:   dur("RELAY_BATCH_TIMEOUT", 30*time.Second, &errs),
			Listen:         boolean("RELAY_LISTEN", true, &errs),
			Retention:      dur("OUTBOX_RETENTION", 7*24*time.Hour, &errs),
			CleanupEvery:   dur("OUTBOX_CLEANUP_INTERVAL", time.Hour, &errs),
		},
		ServiceName:      env("OTEL_SERVICE_NAME", "goapi-relay"),
		TracingEnabled:   env("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "" || env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "") != "",
		TraceSampleRatio: float("TRACE_SAMPLE_RATIO", 1.0, &errs),
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.NATSURL == "" && c.Env == "prod" {
		errs = append(errs, errors.New("NATS_URL is required when APP_ENV=prod (without it events are only logged and then marked as published)"))
	}
	if c.Relay.BatchSize < 1 || c.Relay.BatchSize > 10000 {
		errs = append(errs, fmt.Errorf("RELAY_BATCH_SIZE must be in [1, 10000], got %d", c.Relay.BatchSize))
	}
	if len(errs) > 0 {
		return config{}, fmt.Errorf("invalid config: %v", errs)
	}
	return c, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("relay exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.Env == "dev" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(telemetry.NewTraceLogHandler(h))
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

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// 不挂 SQL 追踪：每秒的轮询查询会产生大量无意义的根 span
	pool, err := store.NewPool(connectCtx, store.PoolConfig{
		URL: cfg.DatabaseURL, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: 30 * time.Minute, MaxConnIdleTime: 5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	var pub outbox.Publisher
	if cfg.NATSURL == "" {
		logger.Warn("NATS_URL not set: events are only logged and then marked as published (local development only)")
		pub = outbox.LogPublisher{Logger: logger}
	} else {
		nc, err := nats.Connect(cfg.NATSURL,
			nats.Name("goapi-relay"),
			nats.MaxReconnects(-1), // 一直重连；断线期间发布失败，relay 会退避重试
			nats.ReconnectWait(2*time.Second),
			nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { logger.Warn("nats disconnected", "err", err) }),
			nats.ReconnectHandler(func(c *nats.Conn) { logger.Info("nats reconnected", "server", c.ConnectedUrlRedacted()) }),
		)
		if err != nil {
			return fmt.Errorf("connect nats: %w", err)
		}
		defer func() { _ = nc.Drain() }()

		pub, err = outbox.NewNATSPublisher(connectCtx, nc, outbox.NATSOptions{
			Stream: cfg.NATSStream, SubjectPrefix: cfg.NATSPrefix, Replicas: cfg.NATSReplicas,
			MaxAge: cfg.NATSMaxAge, Duplicates: cfg.NATSDuplicates,
		})
		if err != nil {
			return err
		}
		logger.Info("publishing to nats jetstream", "stream", cfg.NATSStream, "subjects", cfg.NATSPrefix+".>")
	}

	reg := telemetry.NewRegistry()
	relay := outbox.New(pool, pub, cfg.Relay, logger, outbox.NewMetrics(reg, pool))

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg, ErrorHandling: promhttp.ContinueOnError}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !relay.Healthy(2 * time.Minute) {
			http.Error(w, "relay loop stalled", http.StatusServiceUnavailable)
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

	logger.Info("relay started", "batch_size", cfg.Relay.BatchSize, "poll_interval", cfg.Relay.PollInterval,
		"listen", cfg.Relay.Listen, "retention", cfg.Relay.Retention)
	runErr := relay.Run(ctx)

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = metricsSrv.Shutdown(shutdownCtx)
	logger.Info("relay stopped")
	return runErr
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

func boolean(key string, def bool, errs *[]error) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}
