// Command api 是 goapi 的 HTTP 服务。
//
//	api            启动 HTTP 服务
//	api migrate    只执行数据库迁移然后退出，供 Kubernetes Job 使用
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"

	"ELOATS/goapi/internal/auth"
	"ELOATS/goapi/internal/config"
	"ELOATS/goapi/internal/httpserver"
	"ELOATS/goapi/internal/ratelimit"
	"ELOATS/goapi/internal/service"
	"ELOATS/goapi/internal/store"
	"ELOATS/goapi/internal/telemetry"
)

func main() {
	// `api migrate`：只执行数据库迁移然后退出，供 Kubernetes Job / initContainer 使用。
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(); err != nil {
			slog.Error("migration failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func runMigrate() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required for migrate")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	pool, err := store.NewPool(ctx, store.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        2,
		MinConns:        0,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	logger.Info("database migrations applied")
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)
	if len(cfg.CursorKey) == 0 {
		logger.Warn("CURSOR_SECRET not set: using a random key, pagination cursors become invalid after restart and are not accepted by other replicas")
	}

	// 收到 SIGINT / SIGTERM 时取消 ctx，触发优雅关闭。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 最先初始化、最后关闭：保证退出前的 span 都能刷出去。
	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled:     cfg.TracingEnabled,
		ServiceName: cfg.ServiceName,
		Env:         cfg.Env,
		SampleRatio: cfg.TraceSampleRatio,
	})
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			logger.Warn("flush traces", "err", err)
		}
	}()

	reg := telemetry.NewRegistry()

	deps, closeDeps, err := buildDeps(ctx, cfg, logger, reg)
	if err != nil {
		return err
	}
	// 在 HTTP 服务器完全停止后再关闭连接池（defer 后进先出）。
	defer closeDeps()
	deps.Metrics = telemetry.NewHTTPMetrics(reg)
	if deps.Auth, err = buildAuth(cfg, logger); err != nil {
		return err
	}
	rl, closeRateLimit, err := buildRateLimit(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer closeRateLimit()
	deps.RateLimitIP, deps.RateLimitUser, deps.RateLimitWrite = rl.ip, rl.user, rl.write
	deps.RateLimitMetrics = ratelimit.NewMetrics(reg)

	srv := httpserver.New(cfg, logger, deps)

	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		metricsSrv = newMetricsServer(cfg, reg)
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("http server starting", "addr", cfg.HTTPAddr, "env", cfg.Env,
			"tracing", cfg.TracingEnabled)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if metricsSrv != nil {
		go func() {
			logger.Info("metrics server starting", "addr", cfg.MetricsAddr, "pprof", cfg.PprofEnabled)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics server: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err // 启动失败，例如端口被占用
	case <-ctx.Done():
	}
	stop() // 再按一次 Ctrl+C 可强制退出

	logger.Info("shutdown signal received, draining")
	// 1) 先让 /readyz 返回 503，给负载均衡时间摘掉本实例
	srv.SetNotReady()
	time.Sleep(cfg.ShutdownDrainDelay)

	// 2) 停止接收新连接，等待在途请求完成
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// 3) 指标服务最后关，关闭过程中仍可被抓取
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics server shutdown", "err", err)
		}
	}
	logger.Info("server stopped cleanly")
	return nil
}

// buildDeps 根据配置选择存储实现：有 DATABASE_URL 用 Postgres，否则用内存。
func buildDeps(ctx context.Context, cfg config.Config, logger *slog.Logger, reg prometheus.Registerer) (httpserver.Deps, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("DATABASE_URL not set, using in-memory store (data is lost on restart)")
		svc := service.NewUserService(store.NewMemoryUserStore(), store.NewMemoryEventStore(), store.NoopTxManager{})
		return httpserver.Deps{Users: svc}, func() {}, nil
	}

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := store.NewPool(connectCtx, store.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLife,
		MaxConnIdleTime: cfg.DBMaxConnIdle,
		Tracer:          otelpgx.NewTracer(), // 每条 SQL 一个 span，挂在当前请求的 trace 下
	})
	if err != nil {
		return httpserver.Deps{}, nil, fmt.Errorf("connect postgres: %w", err)
	}
	reg.MustRegister(telemetry.NewPoolCollector(pool))

	if cfg.MigrateOnStart {
		if err := store.Migrate(connectCtx, pool); err != nil {
			pool.Close()
			return httpserver.Deps{}, nil, fmt.Errorf("migrate: %w", err)
		}
		logger.Info("database migrations applied")
	}

	svc := service.NewUserService(
		store.NewPostgresUserStore(pool, cfg.DBQueryTimeout),
		store.NewPostgresEventStore(pool, cfg.DBQueryTimeout),
		store.NewTxManager(pool),
	)
	logger.Info("connected to postgres", "max_conns", cfg.DBMaxConns)
	return httpserver.Deps{
		Users:       svc,
		HealthCheck: pool.Ping,
	}, pool.Close, nil
}

// buildAuth 在启动时做 OIDC discovery：身份提供方不可达就直接退出，而不是带着坏配置跑起来。
func buildAuth(cfg config.Config, logger *slog.Logger) (*auth.Authenticator, error) {
	if cfg.AuthDisabled {
		logger.Warn("AUTH_DISABLED=true: every API request is treated as a local admin, never use this outside local development")
		dev := auth.Principal{Subject: "local-dev", Roles: []string{httpserver.RoleAdmin, httpserver.RoleViewer}}
		return auth.Disabled(dev, logger), nil
	}
	v, err := auth.NewOIDCVerifier(auth.OIDCConfig{
		IssuerURL:    cfg.OIDCIssuerURL,
		Audience:     cfg.OIDCAudience,
		RolesClaim:   cfg.OIDCRolesClaim,
		DiscoveryURL: cfg.OIDCDiscoveryURL,
	})
	if err != nil {
		return nil, fmt.Errorf("setup auth: %w", err)
	}
	logger.Info("oidc authentication enabled", "issuer", cfg.OIDCIssuerURL,
		"audience", cfg.OIDCAudience, "roles_claim", cfg.OIDCRolesClaim, "discovery_url", cfg.OIDCDiscoveryURL)
	return auth.New(v, logger), nil
}

// rateLimiters 是三条限流规则；限流关闭时都为 nil。
type rateLimiters struct {
	ip, user, write ratelimit.Limiter
}

// buildRateLimit 有 REDIS_URL 时用 Redis 共享计数，否则每个实例单独计数。
// 启动时 Redis 连不上就退出；运行中 Redis 出错则放行请求（见 ratelimit.Middleware）。
func buildRateLimit(ctx context.Context, cfg config.Config, logger *slog.Logger) (rateLimiters, func(), error) {
	noop := func() {}
	if !cfg.RateLimitEnabled {
		logger.Warn("rate limiting disabled (RATE_LIMIT_ENABLED=false)")
		return rateLimiters{}, noop, nil
	}
	policies := []ratelimit.Policy{
		{Name: "ip", Rate: cfg.RateLimitIPRate, Burst: cfg.RateLimitIPBurst},
		{Name: "user", Rate: cfg.RateLimitUserRate, Burst: cfg.RateLimitUserBurst},
		{Name: "write", Rate: cfg.RateLimitWriteRate, Burst: cfg.RateLimitWriteBurst},
	}

	backend := "memory"
	closeFn := noop
	newLimiter := func(p ratelimit.Policy) (ratelimit.Limiter, error) { return ratelimit.NewMemory(p) }

	if cfg.RedisURL == "" {
		logger.Warn("REDIS_URL not set, rate limits are counted per instance (effective limit = limit x replicas)")
	} else {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return rateLimiters{}, noop, errors.New("parse REDIS_URL: invalid URL") // 不回显 URL，里面可能有密码
		}
		// 限流在每个请求的关键路径上：超时要短，宁可放行也不要拖慢请求
		opt.DialTimeout = 2 * time.Second
		opt.ReadTimeout = 200 * time.Millisecond
		opt.WriteTimeout = 200 * time.Millisecond
		client := redis.NewClient(opt)
		closeFn = func() { _ = client.Close() }

		// 每次限流判定（EVALSHA）一个子 span，挂在当前请求的 trace 下
		if err := redisotel.InstrumentTracing(client); err != nil {
			closeFn()
			return rateLimiters{}, noop, fmt.Errorf("redis tracing: %w", err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := client.Ping(pingCtx).Err(); err != nil {
			closeFn()
			return rateLimiters{}, noop, fmt.Errorf("connect redis: %w", err)
		}
		backend = "redis"
		newLimiter = func(p ratelimit.Policy) (ratelimit.Limiter, error) { return ratelimit.NewRedis(client, p, "goapi:rl:") }
	}

	ls := make([]ratelimit.Limiter, len(policies))
	for i, p := range policies {
		l, err := newLimiter(p)
		if err != nil {
			closeFn()
			return rateLimiters{}, noop, err
		}
		ls[i] = l
	}
	logger.Info("rate limiting enabled", "backend", backend,
		"ip_rps", cfg.RateLimitIPRate, "ip_burst", cfg.RateLimitIPBurst,
		"user_rps", cfg.RateLimitUserRate, "user_burst", cfg.RateLimitUserBurst,
		"write_rps", cfg.RateLimitWriteRate, "write_burst", cfg.RateLimitWriteBurst)
	return rateLimiters{ip: ls[0], user: ls[1], write: ls[2]}, closeFn, nil
}

// newMetricsServer 在独立的内部端口上暴露 /metrics 和可选的 pprof，
// 不要把这个端口暴露到公网（k8s 里不要被对外的 HTTPRoute 引用）。
func newMetricsServer(cfg config.Config, reg *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	if cfg.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	return &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      65 * time.Second, // 需大于 CPU profile 默认采样时长 30s
		IdleTimeout:       120 * time.Second,
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler
	if cfg.Env == "dev" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(telemetry.NewTraceLogHandler(h))
}
