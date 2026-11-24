package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ---------------- 飞书

// Feishu 对应飞书群的「自定义机器人」。开启「签名校验」后填写 Secret。
// 频率限制约 100 次/分钟、5 次/秒，依靠 Alertmanager 的分组来控制消息量。
type Feishu struct {
	WebhookURL string
	Secret     string
	Client     *http.Client
	Logger     *slog.Logger
	Now        func() time.Time // 测试用，nil 表示 time.Now
}

func (f *Feishu) Send(ctx context.Context, n Notification) error {
	md := n.Markdown("\n")
	if f.WebhookURL == "" {
		f.Logger.InfoContext(ctx, "dry-run: FEISHU_WEBHOOK_URL not set", "title", n.Title, "markdown", md)
		return nil
	}

	body := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"title":    map[string]string{"tag": "plain_text", "content": n.Title},
				"template": cardColor(n),
			},
			"elements": []any{map[string]string{"tag": "markdown", "content": md}},
		},
	}
	if f.Secret != "" {
		ts := strconv.FormatInt(now(f.Now).Unix(), 10)
		body["timestamp"] = ts
		body["sign"] = feishuSign(ts, f.Secret)
	}

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := postJSON(ctx, f.Client, f.WebhookURL, body, &resp); err != nil {
		return fmt.Errorf("feishu: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("feishu: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// feishuSign：以 "timestamp\nsecret" 为 key，对空消息做 HMAC-SHA256，再 base64。
func feishuSign(ts, secret string) string {
	h := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func cardColor(n Notification) string {
	switch {
	case n.Resolved:
		return "green"
	case n.Severity == "critical":
		return "red"
	case n.Severity == "warning":
		return "orange"
	default:
		return "blue"
	}
}

// ---------------- 钉钉

// DingTalk 对应钉钉群的「自定义机器人」。安全设置选「加签」后填写 Secret。
// 频率限制为 20 条/分钟，超过会被限流 10 分钟。
type DingTalk struct {
	WebhookURL string
	Secret     string
	Client     *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
}

func (d *DingTalk) Send(ctx context.Context, n Notification) error {
	md := n.Markdown("\n\n")
	if d.WebhookURL == "" {
		d.Logger.InfoContext(ctx, "dry-run: DINGTALK_WEBHOOK_URL not set", "title", n.Title, "markdown", md)
		return nil
	}

	target := d.WebhookURL
	if d.Secret != "" {
		u, err := url.Parse(d.WebhookURL)
		if err != nil {
			return errors.New("dingtalk: invalid DINGTALK_WEBHOOK_URL") // 不回显 URL，里面有凭据
		}
		ts := strconv.FormatInt(now(d.Now).UnixMilli(), 10)
		q := u.Query()
		q.Set("timestamp", ts)
		q.Set("sign", dingtalkSign(ts, d.Secret))
		u.RawQuery = q.Encode()
		target = u.String()
	}

	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": n.Title,
			"text":  "#### " + n.Title + "\n\n" + md,
		},
	}
	var resp struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := postJSON(ctx, d.Client, target, body, &resp); err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}
	if resp.ErrCode != 0 {
		return fmt.Errorf("dingtalk: errcode=%d errmsg=%s", resp.ErrCode, resp.ErrMsg)
	}
	return nil
}

// dingtalkSign：以 secret 为 key，对 "timestamp\nsecret" 做 HMAC-SHA256，再 base64（URL 编码由调用方完成）。
func dingtalkSign(tsMillis, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(tsMillis + "\n" + secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ---------------- 公共

func postJSON(ctx context.Context, c *http.Client, target string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return errors.New("build request failed") // 不回显 URL
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := c.Do(req)
	if err != nil {
		// *url.Error 的文本里带完整 URL（含 webhook 凭据），只保留底层原因
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s request: %w", ue.Op, ue.Err)
		}
		return err
	}
	defer func() { _ = res.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(data), 300))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func now(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
