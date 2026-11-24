// Package domain 存放核心模型与领域错误，不依赖任何 HTTP / DB 包。
package domain

import (
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalidCursor = errors.New("invalid cursor")
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// UserCursor 是 keyset 分页的位置：排序键 (created_at DESC, id DESC) 上的最后一条记录。
type UserCursor struct {
	CreatedAt time.Time
	ID        string
}

type ListUsersParams struct {
	Limit int         // <=0 使用 DefaultPageSize，超过 MaxPageSize 会被截断
	After *UserCursor // nil 表示第一页
}

type UserPage struct {
	Items []User
	Next  *UserCursor // nil 表示没有下一页
}

// Event 写入 outbox 表，与业务数据在同一事务中提交（transactional outbox）。
type Event struct {
	AggregateID string
	Type        string
	Payload     []byte // JSON
	Actor       string // 操作者（token 的 sub），为空表示系统操作
}
