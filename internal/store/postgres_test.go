package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ELOATS/goapi/internal/domain"
)

// 集成测试：设置 TEST_DATABASE_URL 才会运行，例如
//   TEST_DATABASE_URL=postgres://app:app@localhost:15432/app_test?sslmode=disable go test ./internal/store/
// 注意：会清空 users 和 outbox_events 表，不要指向生产库。
func newTestPG(t *testing.T) (*pgxpool.Pool, *PostgresUserStore) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, PoolConfig{URL: url, MaxConns: 4, MinConns: 0,
		MaxConnLifetime: time.Minute, MaxConnIdleTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE users, outbox_events"); err != nil {
		t.Fatal(err)
	}
	return pool, NewPostgresUserStore(pool, 5*time.Second)
}

func TestPostgresUserStore(t *testing.T) {
	_, s := newTestPG(t)
	ctx := context.Background()

	u, err := s.Create(ctx, "Alice", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "alice@example.com" {
		t.Fatalf("email = %q", got.Email)
	}
	if _, err := s.Create(ctx, "Alice2", "ALICE@example.com"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate email err = %v, want ErrConflict", err)
	}
	if _, err := s.Get(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invalid id err = %v, want ErrNotFound", err)
	}
}

func TestPostgresKeysetPagination(t *testing.T) {
	pool, s := newTestPG(t)
	ctx := context.Background()

	// 同一事务内的 now() 相同：专门验证 created_at 相等时靠 id 打破平局。
	tm := NewTxManager(pool)
	if err := tm.WithTx(ctx, func(ctx context.Context) error {
		for _, e := range []string{"a@x.io", "b@x.io", "c@x.io", "d@x.io", "e@x.io"} {
			if _, err := s.Create(ctx, "u", e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var after *domain.UserCursor
	for page := 0; ; page++ {
		rows, err := s.List(ctx, 2, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, u := range rows {
			if seen[u.ID] {
				t.Fatalf("duplicate %s", u.ID)
			}
			seen[u.ID] = true
		}
		last := rows[len(rows)-1]
		after = &domain.UserCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("seen %d users, want 5", len(seen))
	}

	if _, err := s.List(ctx, 2, &domain.UserCursor{CreatedAt: time.Now(), ID: "bad"}); !errors.Is(err, domain.ErrInvalidCursor) {
		t.Fatalf("bad cursor err = %v, want ErrInvalidCursor", err)
	}
}

func TestTxRollbackAndCommit(t *testing.T) {
	pool, s := newTestPG(t)
	events := NewPostgresEventStore(pool, 5*time.Second)
	tm := NewTxManager(pool)
	ctx := context.Background()

	// 回滚：fn 返回错误，用户不应存在
	var rolledBackID string
	err := tm.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.Create(ctx, "Tx", "tx@example.com")
		if err != nil {
			return err
		}
		rolledBackID = u.ID
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, err := s.Get(ctx, rolledBackID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after rollback err = %v, want ErrNotFound", err)
	}

	// 提交：用户和事件一起落库
	err = tm.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.Create(ctx, "Tx", "tx@example.com")
		if err != nil {
			return err
		}
		return events.Append(ctx, domain.Event{AggregateID: u.ID, Type: "user.created", Payload: []byte(`{"ok":true}`)})
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}
