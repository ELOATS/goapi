// Package service 是业务层：编排仓储、管理事务边界。
// 它只依赖 domain 和自己定义的接口，不知道底层是 Postgres 还是内存。
package service

import (
	"context"
	"encoding/json"
	"fmt"

	"ELOATS/goapi/internal/domain"
)

type UserRepo interface {
	Get(ctx context.Context, id string) (domain.User, error)
	Create(ctx context.Context, name, email string) (domain.User, error)
	// List 按 (created_at DESC, id DESC) 返回 after 之后最多 limit 条。
	List(ctx context.Context, limit int, after *domain.UserCursor) ([]domain.User, error)
}

type EventRepo interface {
	Append(ctx context.Context, e domain.Event) error
}

// TxManager 在一个事务中执行 fn。fn 内部必须使用传入的 ctx，
// 仓储会从 ctx 中取出事务；fn 返回 error 或 panic 时回滚。
type TxManager interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type UserService struct {
	users  UserRepo
	events EventRepo
	tx     TxManager
}

func NewUserService(users UserRepo, events EventRepo, tx TxManager) *UserService {
	return &UserService{users: users, events: events, tx: tx}
}

func (s *UserService) Get(ctx context.Context, id string) (domain.User, error) {
	return s.users.Get(ctx, id)
}

// Create 在同一个事务里写入用户和 user.created 事件：
// 要么都成功，要么都回滚，不会出现"用户建了但事件丢了"。
func (s *UserService) Create(ctx context.Context, name, email string) (domain.User, error) {
	var created domain.User
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.users.Create(ctx, name, email)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(u)
		if err != nil {
			return fmt.Errorf("marshal user.created: %w", err)
		}
		if err := s.events.Append(ctx, domain.Event{
			AggregateID: u.ID,
			Type:        "user.created",
			Payload:     payload,
			Actor:       domain.ActorFrom(ctx),
		}); err != nil {
			return err
		}
		created = u
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return created, nil
}

// List 多取一条（limit+1）来判断是否还有下一页，避免额外的 COUNT 查询。
func (s *UserService) List(ctx context.Context, p domain.ListUsersParams) (domain.UserPage, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = domain.DefaultPageSize
	}
	if limit > domain.MaxPageSize {
		limit = domain.MaxPageSize
	}

	rows, err := s.users.List(ctx, limit+1, p.After)
	if err != nil {
		return domain.UserPage{}, err
	}

	page := domain.UserPage{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[limit-1]
		page.Next = &domain.UserCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if page.Items == nil {
		page.Items = []domain.User{} // JSON 输出 [] 而不是 null
	}
	return page, nil
}
