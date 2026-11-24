package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ReplayDeadLetters 把某个消费者的死信重新发布到原主题，发布成功后删除死信记录，返回重放条数。
// 修复了导致失败的原因之后再调用。
//
// 重放的消息会被这个主题上的所有消费者收到；已经处理过的消费者会按 Event-Id 去重，不受影响。
// 发布后、删除前崩溃，再次重放同一条也一样会被去重。
func ReplayDeadLetters(ctx context.Context, pool *pgxpool.Pool, js jetstream.JetStream, consumerName string, limit int) (int, error) {
	type row struct {
		id, eventID int64
		subject     string
		headers     []byte
		payload     []byte
	}
	rows, err := pool.Query(ctx, `SELECT id, event_id, subject, headers, payload
		FROM consumer_dead_letters WHERE consumer = $1 ORDER BY id LIMIT $2`, consumerName, limit)
	if err != nil {
		return 0, fmt.Errorf("select dead letters: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.eventID, &x.subject, &x.headers, &x.payload)
		return x, err
	})
	if err != nil {
		return 0, fmt.Errorf("scan dead letters: %w", err)
	}

	n := 0
	for _, it := range items {
		msg := nats.NewMsg(it.subject)
		msg.Data = it.payload
		var h nats.Header
		if err := json.Unmarshal(it.headers, &h); err == nil && h != nil {
			msg.Header = h
		}
		msg.Header.Del("Nats-Msg-Id") // 原来的 ID 可能还在去重窗口内，会被 JetStream 丢掉
		msg.Header.Set("Replayed-At", time.Now().UTC().Format(time.RFC3339))

		msgID := fmt.Sprintf("replay-%s-%d-%d", consumerName, it.eventID, time.Now().UnixNano())
		if _, err := js.PublishMsg(ctx, msg, jetstream.WithMsgID(msgID)); err != nil {
			return n, fmt.Errorf("republish event %d: %w", it.eventID, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM consumer_dead_letters WHERE id = $1`, it.id); err != nil {
			return n, fmt.Errorf("delete dead letter %d: %w", it.id, err)
		}
		n++
	}
	return n, nil
}
