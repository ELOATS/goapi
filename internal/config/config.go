// Package config 从环境变量读取配置（12-factor），不引入额外依赖。
package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Env      string // dev | staging | prod
	HTTPAddr string
	LogLevel slog.Level

	// 服务器超时：默认 http.Server 全部为 0（不限制），生产环境必须设置。
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	HandlerTimeout    time.Duration // 单个 API 请求的 context 超时

	ShutdownDrainDelay time.Duration // 摘流量等待时间
	ShutdownTimeout    time.Duration // 等待在途请求的最长时间

	MaxBodyBytes int64
	TrustProxy   bool // 仅在可信反向代理之后才为 true，否则客户端 IP 可被伪造
	// TrustProxy 为 true 时，从这个头读取客户端 IP。必须是代理自己设置、会覆盖客户端同名头的单值头，
	// 例如 Envoy 的 X-Envoy-External-Address、nginx 里 proxy_set_header 设置的 X-Real-IP、Cloudflare 的 CF-Connecting-IP
	ClientIPHeader string

	// 数据库：DATABASE_URL 为空时使用内存存储（方便本地起步）。
	DatabaseURL    string
	DBMaxConns     int32
	DBMinConns     int32
	DBMaxConnLife  time.Duration
	DBMaxConnIdle  time.Duration
	DBQueryTimeout time.Duration // 单条 SQL 的最长执行时间
	MigrateOnStart bool          // 多副本部署建议改为 false，用单独的 Job 跑迁移

	// 可观测性
	ServiceName      string  // OTEL_SERVICE_NAME
	TracingEnabled   bool    // 设置了 OTEL_EXPORTER_OTLP_(TRACES_)ENDPOINT 时自动开启
	TraceSampleRatio float64 // 0–1，按 trace 采样；上游已采样的请求始终跟随（ParentBased）
	MetricsAddr      string  // 内部端口，暴露 /metrics（和可选的 pprof）；为空则不启动
	PprofEnabled     bool
	DocsEnabled      bool // 提供 /api/openapi.yaml 和 /api/docs（Swagger UI）

	// 认证
	OIDCIssuerURL    string
	OIDCAudience     string
	OIDCRolesClaim   string
	OIDCDiscoveryURL string // 可选，见 auth.OIDCConfig.DiscoveryURL
	AuthDisabled     bool   // 仅本地开发；APP_ENV=prod 时禁止

	// 限流
	RateLimitEnabled    bool
	RateLimitIPRate     float64
	RateLimitIPBurst    int
	RateLimitUserRate   float64
	RateLimitUserBurst  int
	RateLimitWriteRate  float64 // 写接口单独的额度，按用户计数
	RateLimitWriteBurst int
	RedisURL            string // 为空则每个实例单独计数

	// 分页游标的签名密钥（至少 32 字节，所有副本共用）。为空时本地随机生成，生产环境必须配置
	CursorKey          []byte
	CursorKeysPrevious [][]byte // 轮换密钥时的旧密钥，旧游标在过渡期内仍然有效
}

func Load() (Config, error) {
	var errs []error
	c := Config{
		Env:                 str("APP_ENV", "dev"),
		HTTPAddr:            str("HTTP_ADDR", ":8080"),
		ReadHeaderTimeout:   dur("HTTP_READ_HEADER_TIMEOUT", 5*time.Second, &errs),
		ReadTimeout:         dur("HTTP_READ_TIMEOUT", 15*time.Second, &errs),
		WriteTimeout:        dur("HTTP_WRITE_TIMEOUT", 30*time.Second, &errs),
		IdleTimeout:         dur("HTTP_IDLE_TIMEOUT", 120*time.Second, &errs),
		HandlerTimeout:      dur("HTTP_HANDLER_TIMEOUT", 20*time.Second, &errs),
		ShutdownDrainDelay:  dur("SHUTDOWN_DRAIN_DELAY", 5*time.Second, &errs),
		ShutdownTimeout:     dur("SHUTDOWN_TIMEOUT", 25*time.Second, &errs),
		MaxBodyBytes:        int64(num("HTTP_MAX_BODY_BYTES", 1<<20, &errs)),
		TrustProxy:          boolean("HTTP_TRUST_PROXY", false, &errs),
		ClientIPHeader:      str("HTTP_CLIENT_IP_HEADER", "X-Real-IP"),
		DatabaseURL:         str("DATABASE_URL", ""),
		DBMaxConns:          num32("DB_MAX_CONNS", 10, &errs),
		DBMinConns:          num32("DB_MIN_CONNS", 2, &errs),
		DBMaxConnLife:       dur("DB_MAX_CONN_LIFETIME", 30*time.Minute, &errs),
		DBMaxConnIdle:       dur("DB_MAX_CONN_IDLE", 5*time.Minute, &errs),
		DBQueryTimeout:      dur("DB_QUERY_TIMEOUT", 5*time.Second, &errs),
		MigrateOnStart:      boolean("DB_MIGRATE_ON_START", true, &errs),
		ServiceName:         str("OTEL_SERVICE_NAME", "goapi"),
		TracingEnabled:      str("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "" || str("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "") != "",
		TraceSampleRatio:    float("TRACE_SAMPLE_RATIO", 1.0, &errs),
		MetricsAddr:         str("METRICS_ADDR", ":9090"),
		PprofEnabled:        boolean("PPROF_ENABLED", false, &errs),
		DocsEnabled:         boolean("DOCS_ENABLED", true, &errs),
		OIDCIssuerURL:       str("OIDC_ISSUER_URL", ""),
		OIDCAudience:        str("OIDC_AUDIENCE", ""),
		OIDCRolesClaim:      str("OIDC_ROLES_CLAIM", "roles"),
		OIDCDiscoveryURL:    str("OIDC_DISCOVERY_URL", ""),
		AuthDisabled:        boolean("AUTH_DISABLED", false, &errs),
		RateLimitEnabled:    boolean("RATE_LIMIT_ENABLED", true, &errs),
		RateLimitIPRate:     float("RATE_LIMIT_IP_RPS", 20, &errs),
		RateLimitIPBurst:    num("RATE_LIMIT_IP_BURST", 40, &errs),
		RateLimitUserRate:   float("RATE_LIMIT_USER_RPS", 10, &errs),
		RateLimitUserBurst:  num("RATE_LIMIT_USER_BURST", 20, &errs),
		RateLimitWriteRate:  float("RATE_LIMIT_WRITE_RPS", 2, &errs),
		RateLimitWriteBurst: num("RATE_LIMIT_WRITE_BURST", 10, &errs),
		CursorKey:           []byte(str("CURSOR_SECRET", "")),
		RedisURL:            str("REDIS_URL", ""),
	}
	if c.RateLimitEnabled {
		if c.RateLimitIPRate <= 0 || c.RateLimitUserRate <= 0 || c.RateLimitWriteRate <= 0 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_IP_RPS, RATE_LIMIT_USER_RPS and RATE_LIMIT_WRITE_RPS must be > 0"))
		}
		if c.RateLimitIPBurst < 1 || c.RateLimitUserBurst < 1 || c.RateLimitWriteBurst < 1 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_IP_BURST, RATE_LIMIT_USER_BURST and RATE_LIMIT_WRITE_BURST must be >= 1"))
		}
	}
	if v := str("CURSOR_SECRET_PREVIOUS", ""); v != "" {
		c.CursorKeysPrevious = [][]byte{[]byte(v)}
	}
	if len(c.CursorKey) == 0 && c.Env == "prod" {
		errs = append(errs, fmt.Errorf("CURSOR_SECRET is required when APP_ENV=prod: all replicas must share it, otherwise cursors from one replica are rejected by another"))
	}
	if n := len(c.CursorKey); n > 0 && n < 32 {
		errs = append(errs, fmt.Errorf("CURSOR_SECRET must be at least 32 bytes, got %d", n))
	}
	for _, k := range c.CursorKeysPrevious {
		if len(k) < 32 {
			errs = append(errs, fmt.Errorf("CURSOR_SECRET_PREVIOUS must be at least 32 bytes, got %d", len(k)))
		}
	}
	// 认证默认开启，必须显式关闭；生产环境不允许关闭
	if c.AuthDisabled {
		if c.Env == "prod" {
			errs = append(errs, fmt.Errorf("AUTH_DISABLED=true is not allowed when APP_ENV=prod"))
		}
	} else {
		if c.OIDCIssuerURL == "" {
			errs = append(errs, fmt.Errorf("OIDC_ISSUER_URL is required (for local development set AUTH_DISABLED=true)"))
		}
		if c.OIDCAudience == "" {
			errs = append(errs, fmt.Errorf("OIDC_AUDIENCE is required, otherwise tokens issued to other applications would be accepted"))
		}
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		errs = append(errs, fmt.Errorf("TRACE_SAMPLE_RATIO (%v) must be between 0 and 1", c.TraceSampleRatio))
	}
	if c.MetricsAddr != "" && c.MetricsAddr == c.HTTPAddr {
		errs = append(errs, fmt.Errorf("METRICS_ADDR must differ from HTTP_ADDR"))
	}
	if c.DBMinConns > c.DBMaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS (%d) must be <= DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns))
	}
	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.HandlerTimeout >= c.WriteTimeout {
		errs = append(errs, fmt.Errorf("HTTP_HANDLER_TIMEOUT (%s) must be < HTTP_WRITE_TIMEOUT (%s)", c.HandlerTimeout, c.WriteTimeout))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid config: %v", errs)
	}
	return c, nil
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func dur(key string, def time.Duration, errs *[]error) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func num32(key string, def int32, errs *[]error) int32 {
	n := num(key, int(def), errs)
	if n < math.MinInt32 || n > math.MaxInt32 {
		*errs = append(*errs, fmt.Errorf("%s: %d is out of range", key, n))
		return def
	}
	return int32(n) //nolint:gosec // 上面已经检查过范围
}

func num(key string, def int, errs *[]error) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
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
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
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
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}
