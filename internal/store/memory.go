// Package store 是数据访问层。memory.go 是内存实现，用于本地开发和测试。
package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"ELOATS/goapi/internal/domain"
)

type MemoryUserStore struct {
	mu      sync.RWMutex
	byID    map[string]domain.User
	byEmail map[string]string
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{byID: map[string]domain.User{}, byEmail: map[string]string{}}
}

func (s *MemoryUserStore) Get(ctx context.Context, id string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byID[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (s *MemoryUserStore) Create(ctx context.Context, name, email string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	key := strings.ToLower(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byEmail[key]; exists {
		return domain.User{}, domain.ErrConflict
	}
	u := domain.User{ID: newUUID(), Name: name, Email: email, CreatedAt: time.Now().UTC()}
	s.byID[u.ID] = u
	s.byEmail[key] = u.ID
	return u, nil
}

// List 与 Postgres 实现保持相同语义：(created_at DESC, id DESC)，返回游标之后的记录。
// O(n log n)，仅适合开发和测试。
func (s *MemoryUserStore) List(ctx context.Context, limit int, after *domain.UserCursor) ([]domain.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	all := make([]domain.User, 0, len(s.byID))
	for _, u := range s.byID {
		all = append(all, u)
	}
	s.mu.RUnlock()

	slices.SortFunc(all, func(a, b domain.User) int {
		return -compareKey(a.CreatedAt, a.ID, b.CreatedAt, b.ID)
	})

	out := make([]domain.User, 0, min(limit, len(all)))
	for _, u := range all {
		if after != nil && compareKey(u.CreatedAt, u.ID, after.CreatedAt, after.ID) >= 0 {
			continue
		}
		out = append(out, u)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func compareKey(t1 time.Time, id1 string, t2 time.Time, id2 string) int {
	if c := t1.Compare(t2); c != 0 {
		return c
	}
	return strings.Compare(id1, id2)
}

// MemoryEventStore 只把事件存在切片里。
type MemoryEventStore struct {
	mu     sync.Mutex
	events []domain.Event
}

func NewMemoryEventStore() *MemoryEventStore { return &MemoryEventStore{} }

// Events 返回已写入事件的副本，供测试检查。
func (s *MemoryEventStore) Events() []domain.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func (s *MemoryEventStore) Append(ctx context.Context, e domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// NoopTxManager 直接执行 fn，不提供原子性和回滚：仅用于内存模式。
type NoopTxManager struct{}

func (NoopTxManager) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// newUUID 生成 v4 UUID，格式与 Postgres 实现一致。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // Go 1.24+ 的 crypto/rand.Read 不会返回错误
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
