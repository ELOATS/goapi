// Command alertbridge 把 Alertmanager 的 webhook 转发到飞书 / 钉钉自定义机器人。
//
// Alertmanager 原生不支持飞书和钉钉，这个小服务只依赖标准库：
//   POST /feishu    → 飞书群机器人（消息卡片）
//   POST /dingtalk  → 钉钉群机器人（markdown）
// 对应的 *_WEBHOOK_URL 未设置时进入 dry-run：只把消息打印到日志，方便本地调试。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // distroless 镜像里没有时区数据，编进二进制
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	loc, err := time.LoadLocation(env("ALERT_TIMEZONE", "Asia/Taipei"))
	if err != nil {
		logger.Error("invalid ALERT_TIMEZONE", "err", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 8 * time.Second}
	feishu := &Feishu{
		WebhookURL: os.Getenv("FEISHU_WEBHOOK_URL"),
		Secret:     os.Getenv("FEISHU_SECRET"),
		Client:     client,
		Logger:     logger,
	}
	dingtalk := &DingTalk{
		WebhookURL: os.Getenv("DINGTALK_WEBHOOK_URL"),
		Secret:     os.Getenv("DINGTALK_SECRET"),
		Client:     client,
		Logger:     logger,
	}

	mux := http.NewServeMux()
	mux.Handle("POST /feishu", webhookHandler(feishu, loc, logger))
	mux.Handle("POST /dingtalk", webhookHandler(dingtalk, loc, logger))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              env("BRIDGE_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	idle := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(idle)
	}()

	logger.Info("alertbridge listening", "addr", srv.Addr, "timezone", loc.String(),
		"feishu_configured", feishu.WebhookURL != "", "dingtalk_configured", dingtalk.WebhookURL != "")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	<-idle
}

// Sender 把一条格式化好的通知发到某个 IM。
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// webhookHandler 接收 Alertmanager 的 webhook。
// 返回 5xx 时 Alertmanager 会重试，返回 4xx 不会重试，所以只有"推送失败"才返回 502。
func webhookHandler(s Sender, loc *time.Location, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var m Message
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, "invalid alertmanager payload", http.StatusBadRequest)
			return
		}

		n := Format(m, loc)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if err := s.Send(ctx, n); err != nil {
			logger.ErrorContext(ctx, "notify failed", "path", r.URL.Path, "receiver", m.Receiver,
				"title", n.Title, "err", err)
			http.Error(w, "upstream notify failed", http.StatusBadGateway)
			return
		}
		logger.InfoContext(ctx, "notified", "path", r.URL.Path, "receiver", m.Receiver,
			"title", n.Title, "alerts", len(m.Alerts))
		w.WriteHeader(http.StatusOK)
	})
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
