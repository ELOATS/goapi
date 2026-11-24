package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// 测试向量由独立实现（Python hmac）计算得出。
func TestFeishuSign(t *testing.T) {
	if got, want := feishuSign("1700000000", "unit-test-key"), "bQdmj9mbs4TSv0udPyXmVWo4a4eKgZgZn5GRap3SAXc="; got != want {
		t.Fatalf("feishuSign = %s, want %s", got, want)
	}
}

func TestDingTalkSign(t *testing.T) {
	if got, want := dingtalkSign("1700000000000", "unit-test-key"), "dOyen3qKKBqEn3SiTJFT5Q2nuW/y5wfupWVnxRn3gnw="; got != want {
		t.Fatalf("dingtalkSign = %s, want %s", got, want)
	}
}

func sampleMessage() Message {
	start := time.Date(2026, 11, 23, 3, 24, 5, 0, time.UTC)
	return Message{
		Status:       "firing",
		Receiver:     "oncall",
		GroupLabels:  map[string]string{"alertname": "GoapiDBAcquireCanceled"},
		CommonLabels: map[string]string{"alertname": "GoapiDBAcquireCanceled", "severity": "critical", "service": "goapi"},
		ExternalURL:  "http://localhost:9093",
		Alerts: []Alert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "GoapiDBAcquireCanceled", "service": "goapi", "instance": "api-1:9090"},
				Annotations: map[string]string{"summary": "api-1 有请求等连接超时", "runbook_url": "https://example.com/runbook#x"},
				StartsAt:    start,
			},
			{
				Status:   "firing",
				Labels:   map[string]string{"alertname": "GoapiDBAcquireCanceled", "service": "goapi", "instance": "api-2:9090"},
				StartsAt: start,
			},
			{
				Status:   "resolved",
				Labels:   map[string]string{"alertname": "GoapiDBAcquireCanceled", "service": "goapi", "instance": "api-3:9090"},
				StartsAt: start,
				EndsAt:   start.Add(90 * time.Second),
			},
		},
	}
}

func TestFormat(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	n := Format(sampleMessage(), loc)
	if n.Title != "[CRITICAL:2] GoapiDBAcquireCanceled" {
		t.Fatalf("title = %q", n.Title)
	}
	md := n.Markdown("\n")
	for _, want := range []string{
		"告警中（2）", "已恢复（1）",
		"api-1 有请求等连接超时",
		"instance=api-2:9090",
		"开始 2026-11-23 11:24:05 CST",
		"持续 1m30s",
		"[处理手册](https://example.com/runbook#x)",
		"http://localhost:9093/#/silences/new?filter=",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	// 已恢复的告警不应该带静默链接：3 条里只有 2 条 firing
	if c := strings.Count(md, "[静默]"); c != 2 {
		t.Errorf("silence links = %d, want 2", c)
	}

	m := sampleMessage()
	for i := range m.Alerts {
		m.Alerts[i].Status = "resolved"
	}
	if n := Format(m, loc); !n.Resolved || n.Title != "[已恢复] GoapiDBAcquireCanceled" {
		t.Fatalf("resolved title = %q resolved=%v", n.Title, n.Resolved)
	}
}

func TestMarkdownTruncates(t *testing.T) {
	n := Notification{Lines: []string{strings.Repeat("长", maxMarkdownRunes+100)}}
	if got := len([]rune(n.Markdown("\n"))); got > maxMarkdownRunes+20 {
		t.Fatalf("markdown runes = %d, not truncated", got)
	}
}

func TestFeishuSend(t *testing.T) {
	var got map[string]any
	reply := `{"code":0,"msg":"success"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, reply)
	}))
	defer up.Close()

	f := &Feishu{WebhookURL: up.URL, Secret: "unit-test-key", Client: up.Client(), Logger: discard,
		Now: func() time.Time { return time.Unix(1700000000, 0) }}
	n := Format(sampleMessage(), time.UTC)
	if err := f.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "interactive" || got["timestamp"] != "1700000000" || got["sign"] != "bQdmj9mbs4TSv0udPyXmVWo4a4eKgZgZn5GRap3SAXc=" {
		t.Fatalf("unexpected body: %v", got)
	}
	header := got["card"].(map[string]any)["header"].(map[string]any)
	if header["template"] != "red" {
		t.Fatalf("card color = %v, want red", header["template"])
	}

	reply = `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`
	if err := f.Send(context.Background(), n); err == nil || !strings.Contains(err.Error(), "19021") {
		t.Fatalf("want code 19021 error, got %v", err)
	}
}

func TestDingTalkSend(t *testing.T) {
	var query map[string]string
	var body map[string]any
	reply := `{"errcode":0,"errmsg":"ok"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		query = map[string]string{"timestamp": q.Get("timestamp"), "sign": q.Get("sign"), "keep": q.Get("keep")}
		_ = json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, reply)
	}))
	defer up.Close()

	d := &DingTalk{WebhookURL: up.URL + "/robot/send?keep=1", Secret: "unit-test-key", Client: up.Client(), Logger: discard,
		Now: func() time.Time { return time.UnixMilli(1700000000000) }}
	n := Format(sampleMessage(), time.UTC)
	if err := d.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if query["timestamp"] != "1700000000000" || query["sign"] != "dOyen3qKKBqEn3SiTJFT5Q2nuW/y5wfupWVnxRn3gnw=" || query["keep"] != "1" {
		t.Fatalf("unexpected query: %v", query)
	}
	if body["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v", body["msgtype"])
	}

	reply = `{"errcode":310000,"errmsg":"sign not match"}`
	if err := d.Send(context.Background(), n); err == nil || !strings.Contains(err.Error(), "310000") {
		t.Fatalf("want errcode 310000 error, got %v", err)
	}
}

func TestErrorsDoNotLeakWebhookURL(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	addr := up.URL
	up.Close() // 让请求必然失败

	d := &DingTalk{WebhookURL: addr + "/robot/send?a=very-private-value", Client: &http.Client{Timeout: time.Second}, Logger: discard}
	err := d.Send(context.Background(), Format(sampleMessage(), time.UTC))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "very-private-value") {
		t.Fatalf("error leaks webhook URL: %v", err)
	}
}

func TestDryRunWhenNotConfigured(t *testing.T) {
	for _, s := range []Sender{&Feishu{Logger: discard}, &DingTalk{Logger: discard}} {
		if err := s.Send(context.Background(), Notification{Title: "x"}); err != nil {
			t.Fatalf("%T dry-run err = %v", s, err)
		}
	}
}

type fakeSender struct{ err error }

func (f fakeSender) Send(context.Context, Notification) error { return f.err }

func TestWebhookHandlerStatusCodes(t *testing.T) {
	payload, _ := json.Marshal(sampleMessage())
	cases := []struct {
		name   string
		sender Sender
		body   string
		want   int
	}{
		{"ok", fakeSender{}, string(payload), http.StatusOK},
		{"upstream fails -> 502 so alertmanager retries", fakeSender{errors.New("boom")}, string(payload), http.StatusBadGateway},
		{"bad json -> 400, no retry", fakeSender{}, "{", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/feishu", strings.NewReader(tc.body))
			webhookHandler(tc.sender, time.UTC, discard).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
