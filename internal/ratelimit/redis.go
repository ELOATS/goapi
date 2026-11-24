package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// gcraScript 在 Redis 里原子地执行 GCRA，时间单位是微秒。
// 用 Redis 的 TIME 作为时钟：各副本本地时钟不一致也不影响结果。
// 数字用 %.0f 格式化写回：Lua 默认的 tostring 只保留 14 位有效数字，会丢精度。
// 返回 {1, 剩余额度对应的微秒数, 完全恢复的微秒数} 或 {0, 需要等待的微秒数, 完全恢复的微秒数}。
var gcraScript = redis.NewScript(`
local interval = tonumber(ARGV[1])
local tolerance = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local tat = tonumber(redis.call('GET', KEYS[1])) or now
if tat < now then tat = now end
local new_tat = tat + interval
local diff = new_tat - now
if diff > tolerance then
  return {0, diff - tolerance, tat - now}
end
redis.call('SET', KEYS[1], string.format('%.0f', new_tat), 'PX', string.format('%d', math.ceil(diff / 1000)))
return {1, tolerance - diff, diff}
`)

// Redis 是多副本共享计数的限流器。每个键带 PX 过期时间，额度恢复后自动消失。
type Redis struct {
	client redis.Scripter
	policy Policy
	prefix string
}

func NewRedis(client redis.Scripter, p Policy, prefix string) (*Redis, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Redis{client: client, policy: p, prefix: prefix}, nil
}

func (l *Redis) Policy() Policy { return l.policy }

func (l *Redis) Allow(ctx context.Context, key string) (Result, error) {
	interval := l.policy.interval().Microseconds()
	tolerance := l.policy.tolerance().Microseconds()

	vals, err := gcraScript.Run(ctx, l.client,
		[]string{l.prefix + l.policy.Name + ":" + key}, interval, tolerance).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: redis: %w", err)
	}
	if len(vals) != 3 {
		return Result{}, fmt.Errorf("ratelimit: unexpected redis reply %v", vals)
	}

	r := Result{Limit: l.policy.Burst, ResetAfter: time.Duration(vals[2]) * time.Microsecond}
	if vals[0] == 1 {
		r.Allowed = true
		r.Remaining = int(vals[1] / interval)
	} else {
		r.RetryAfter = time.Duration(vals[1]) * time.Microsecond
	}
	return r, nil
}
