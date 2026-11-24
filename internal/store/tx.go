package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type txKey struct{}

// dbtx 是 *pgxpool.Pool 和 pgx.Tx 的公共子集，仓储只依赖它。
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// conn 优先返回 ctx 中的事务，否则返回连接池。
func conn(ctx context.Context, pool *pgxpool.Pool) dbtx {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return pool
}

type TxManager struct {
	pool *pgxpool.Pool
	opts pgx.TxOptions
}

// NewTxManager 默认使用 READ COMMITTED。需要更强隔离时传入 opts，
// 例如 pgx.TxOptions{IsoLevel: pgx.Serializable}（此时调用方要处理 40001 重试）。
func NewTxManager(pool *pgxpool.Pool, opts ...pgx.TxOptions) *TxManager {
	m := &TxManager{pool: pool}
	if len(opts) > 0 {
		m.opts = opts[0]
	}
	return m
}

// WithTx 开启事务执行 fn：返回 nil 则提交，返回 error 或 panic 则回滚。
// 嵌套调用会加入外层事务，而不是开新事务。
func (m *TxManager) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := m.pool.BeginTx(ctx, m.opts)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// 回滚用不会被取消的 ctx：请求超时后也要能把事务清理干净。
	rollbackCtx := context.WithoutCancel(ctx)

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(rollbackCtx)
			panic(p)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		if rbErr := tx.Rollback(rollbackCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
