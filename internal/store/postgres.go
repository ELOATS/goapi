package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"example.com/goapi/internal/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres 错误码：https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	pgUniqueViolation = "23505"
	pgInvalidTextRepr = "22P02" // 例如非法 UUID 字符串
)

type PoolConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	Tracer          pgx.QueryTracer // 可选，例如 otelpgx.NewTracer()
}

// NewPool 创建连接池并立即 Ping，确保启动阶段就暴露连接问题（fail fast）。
func NewPool(ctx context.Context, c PoolConfig) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err) // 不要把 URL 打进日志，里面有密码
	}
	cfg.MaxConns = c.MaxConns
	cfg.MinConns = c.MinConns
	cfg.MaxConnLifetime = c.MaxConnLifetime
	cfg.MaxConnIdleTime = c.MaxConnIdleTime
	cfg.HealthCheckPeriod = 30 * time.Second
	if c.Tracer != nil {
		cfg.ConnConfig.Tracer = c.Tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate 执行内嵌的 goose 迁移。goose.UpContext 不加分布式锁，
// 多副本同时启动可能并发迁移：生产环境建议关闭 DB_MIGRATE_ON_START，改用单独的迁移任务。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, "migrations")
}

// ---------------- users

type PostgresUserStore struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
}

func NewPostgresUserStore(pool *pgxpool.Pool, queryTimeout time.Duration) *PostgresUserStore {
	return &PostgresUserStore{pool: pool, queryTimeout: queryTimeout}
}

const userColumns = `id::text, name, email, created_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt); err != nil {
		return domain.User{}, err
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return u, nil
}

func (s *PostgresUserStore) Get(ctx context.Context, id string) (domain.User, error) {
	if !isUUID(id) {
		return domain.User{}, domain.ErrNotFound // 非法 ID 对外等同于不存在，也省一次查询
	}
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	row := conn(ctx, s.pool).QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, mapErr(err)
	}
	return u, nil
}

func (s *PostgresUserStore) Create(ctx context.Context, name, email string) (domain.User, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	row := conn(ctx, s.pool).QueryRow(ctx,
		`INSERT INTO users (name, email) VALUES ($1, $2) RETURNING `+userColumns, name, email)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, mapErr(err)
	}
	return u, nil
}

// List 使用 keyset 分页：WHERE (created_at, id) < (游标) ORDER BY created_at DESC, id DESC。
// 和 OFFSET 不同，翻到多深都只扫描 limit 行，且并发插入不会导致跳过或重复。
// 依赖索引 users_created_at_id_idx。
func (s *PostgresUserStore) List(ctx context.Context, limit int, after *domain.UserCursor) ([]domain.User, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = conn(ctx, s.pool).Query(ctx,
			`SELECT `+userColumns+` FROM users
			 ORDER BY created_at DESC, id DESC
			 LIMIT $1`, limit)
	} else {
		if !isUUID(after.ID) {
			return nil, domain.ErrInvalidCursor
		}
		rows, err = conn(ctx, s.pool).Query(ctx,
			`SELECT `+userColumns+` FROM users
			 WHERE (created_at, id) < ($2, $3::uuid)
			 ORDER BY created_at DESC, id DESC
			 LIMIT $1`, limit, after.CreatedAt, after.ID)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	users, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.User, error) {
		return scanUser(r)
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return users, nil
}

// ---------------- outbox events

type PostgresEventStore struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
}

func NewPostgresEventStore(pool *pgxpool.Pool, queryTimeout time.Duration) *PostgresEventStore {
	return &PostgresEventStore{pool: pool, queryTimeout: queryTimeout}
}

func (s *PostgresEventStore) Append(ctx context.Context, e domain.Event) error {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	// 把当前请求的链路上下文存下来，relay 投递时据此把 span 挂回原来的 trace
	var traceContext any // nil 写入 NULL
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		if b, err := json.Marshal(carrier); err == nil {
			traceContext = string(b)
		}
	}

	_, err := conn(ctx, s.pool).Exec(ctx,
		`INSERT INTO outbox_events (aggregate_id, event_type, payload, actor, trace_context)
		 VALUES ($1::uuid, $2, $3::jsonb, NULLIF($4, ''), $5::jsonb)`,
		e.AggregateID, e.Type, string(e.Payload), e.Actor, traceContext)
	if err != nil {
		return fmt.Errorf("append event %s: %w", e.Type, err)
	}
	return nil
}

// ---------------- helpers

// mapErr 把驱动错误翻译成领域错误，让 service / handler 层不依赖 pgx。
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return domain.ErrConflict
		case pgInvalidTextRepr:
			return domain.ErrNotFound
		}
	}
	return err // context.DeadlineExceeded 等原样返回，由 handler 映射
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}
