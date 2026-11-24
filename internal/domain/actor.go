package domain

import "context"

type actorKey struct{}

// WithActor 记录当前操作者（通常是 token 的 sub）。认证中间件负责写入，
// 业务层用 ActorFrom 读取，这样 service / store 不需要依赖 auth 包。
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom 返回当前操作者；没有时返回空字符串（例如后台任务）。
func ActorFrom(ctx context.Context) string {
	s, _ := ctx.Value(actorKey{}).(string)
	return s
}
