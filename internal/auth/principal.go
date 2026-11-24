// Package auth 实现 OIDC / JWT Bearer 认证和基于角色的授权（RBAC）。
package auth

import (
	"context"
	"slices"

	"example.com/goapi/internal/domain"
)

// Principal 是通过认证的调用方。
type Principal struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Roles   []string `json:"roles"`
	Scopes  []string `json:"scopes,omitempty"`
}

// HasAnyRole 拥有任意一个角色即返回 true。
func (p Principal) HasAnyRole(roles ...string) bool {
	for _, r := range roles {
		if slices.Contains(p.Roles, r) {
			return true
		}
	}
	return false
}

type principalKey struct{}

// WithPrincipal 把调用方放进 ctx，同时把 sub 记为业务层的操作者。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	ctx = context.WithValue(ctx, principalKey{}, p)
	return domain.WithActor(ctx, p.Subject)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
