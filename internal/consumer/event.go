// Package consumer 是 JetStream 事件消费者的通用部分：拉取消息、按 Event-Id 去重、
// 在一个数据库事务里执行业务处理、重试和死信、链路传递。业务逻辑只需要实现 Handler。
package consumer

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go"
)

// Event 是从消息头和消息体解析出的事件，字段与 outbox relay 写入的消息头一一对应。
type Event struct {
	ID          int64 // Event-Id，去重用
	Type        string
	AggregateID string
	Actor       string
	OccurredAt  time.Time // 零值表示消息里没有这个头
	Subject     string
	Payload     []byte
	Delivery    uint64 // 第几次投递，从 1 开始
}

// ParseEvent 校验并解析消息。缺少 Event-Id 或 Event-Type 的消息无法去重，按格式错误处理。
func ParseEvent(subject string, h nats.Header, data []byte) (Event, error) {
	idStr := h.Get("Event-Id")
	if idStr == "" {
		return Event{}, errors.New("missing Event-Id header")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return Event{}, fmt.Errorf("invalid Event-Id %q", idStr)
	}
	typ := h.Get("Event-Type")
	if typ == "" {
		return Event{}, errors.New("missing Event-Type header")
	}
	e := Event{
		ID:          id,
		Type:        typ,
		AggregateID: h.Get("Aggregate-Id"),
		Actor:       h.Get("Actor"),
		Subject:     subject,
		Payload:     data,
	}
	if v := h.Get("Occurred-At"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return Event{}, fmt.Errorf("invalid Occurred-At %q", v)
		}
		e.OccurredAt = t
	}
	return e, nil
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent 标记"重试也不会成功"的错误（例如消息内容不合法）：消息直接进死信，不再重试。
// 其它错误都视为暂时性的，会按退避重试，直到达到 MaxDeliver。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// PermanentIfDataError 把 Postgres 的数据异常（22xxx，例如非法 UUID）和约束违反（23xxx）
// 标记为永久错误：同样的数据重试多少次结果都一样。其它错误原样返回。
func PermanentIfDataError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 && (pgErr.Code[:2] == "22" || pgErr.Code[:2] == "23") {
		return Permanent(err)
	}
	return err
}

// Backoff 返回第 attempt 次投递失败后的重试间隔：base、2×base、4×base……，最大 maxDelay。
func Backoff(attempt uint64, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := uint64(1); i < attempt; i++ {
		d *= 2
		if d >= maxDelay {
			return maxDelay
		}
	}
	return min(d, maxDelay)
}
