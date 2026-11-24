package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// WelcomeHandler 是示例业务：收到 user.created 后记录一条待发送的欢迎通知。
// 真正发邮件应该由另一个进程读取 welcome_notifications 去做；直接在这里调邮件接口的话，
// 发送成功但事务提交失败时会重发邮件。
func WelcomeHandler(logger *slog.Logger) Handler {
	return func(ctx context.Context, db DB, e Event) error {
		var u struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal(e.Payload, &u); err != nil {
			return Permanent(fmt.Errorf("decode user.created payload: %w", err))
		}
		if u.ID == "" || u.Email == "" {
			return Permanent(errors.New("user.created payload is missing id or email"))
		}

		// 主键是 user_id：即使去重记录被清理后又收到同一事件，也只会有一条
		_, err := db.Exec(ctx, `INSERT INTO welcome_notifications (user_id, email)
			VALUES ($1::uuid, $2) ON CONFLICT (user_id) DO NOTHING`, u.ID, u.Email)
		if err != nil {
			return PermanentIfDataError(fmt.Errorf("insert welcome notification: %w", err))
		}
		logger.InfoContext(ctx, "welcome notification queued", "user_id", u.ID, "event_id", e.ID, "actor", e.Actor)
		return nil
	}
}
