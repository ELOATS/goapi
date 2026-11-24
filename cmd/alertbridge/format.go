package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Message 是 Alertmanager webhook（version 4）的请求体。
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config
type Message struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"` // firing | resolved
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Notification 是与 IM 无关的中间格式，各 Sender 再渲染成自己的消息结构。
type Notification struct {
	Title    string
	Severity string
	Resolved bool
	Lines    []string
}

const (
	timeLayout       = "2006-01-02 15:04:05 MST"
	maxAlertsShown   = 10   // 每组最多展示多少条，避免一条消息过长
	maxMarkdownRunes = 3500 // 飞书 / 钉钉单条消息都有长度上限，留足余量
)

// Markdown 用 sep 拼接各行：飞书用 "\n"，钉钉的 markdown 需要 "\n\n" 才能可靠换行。
func (n Notification) Markdown(sep string) string {
	s := strings.Join(n.Lines, sep)
	if r := []rune(s); len(r) > maxMarkdownRunes {
		s = string(r[:maxMarkdownRunes]) + sep + "……（内容过长已截断）"
	}
	return s
}

func Format(m Message, loc *time.Location) Notification {
	var firing, resolved []Alert
	for _, a := range m.Alerts {
		if a.Status == "resolved" {
			resolved = append(resolved, a)
		} else {
			firing = append(firing, a)
		}
	}

	name := m.GroupLabels["alertname"]
	if name == "" {
		name = m.CommonLabels["alertname"]
	}
	if name == "" {
		name = "多条告警"
	}

	n := Notification{Severity: m.CommonLabels["severity"], Resolved: len(firing) == 0}
	switch {
	case n.Resolved:
		n.Title = "[已恢复] " + name
	case n.Severity != "":
		n.Title = fmt.Sprintf("[%s:%d] %s", strings.ToUpper(n.Severity), len(firing), name)
	default:
		n.Title = fmt.Sprintf("[告警:%d] %s", len(firing), name)
	}

	n.Lines = append(n.Lines, alertLines("🔥 告警中", firing, m.ExternalURL, loc)...)
	n.Lines = append(n.Lines, alertLines("✅ 已恢复", resolved, m.ExternalURL, loc)...)
	if m.TruncatedAlerts > 0 {
		n.Lines = append(n.Lines, fmt.Sprintf("另有 %d 条告警被 max_alerts 截断", m.TruncatedAlerts))
	}
	return n
}

func alertLines(title string, alerts []Alert, externalURL string, loc *time.Location) []string {
	if len(alerts) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("**%s（%d）**", title, len(alerts))}
	for i, a := range alerts {
		if i == maxAlertsShown {
			lines = append(lines, fmt.Sprintf("…… 其余 %d 条省略", len(alerts)-i))
			break
		}
		head := a.Annotations["summary"]
		if head == "" {
			head = a.Labels["alertname"]
		}
		lines = append(lines, "● "+head)
		if d := a.Annotations["description"]; d != "" {
			lines = append(lines, d)
		}

		var meta []string
		for _, k := range []string{"instance", "route"} {
			if v := a.Labels[k]; v != "" {
				meta = append(meta, k+"="+v)
			}
		}
		if len(meta) > 0 {
			lines = append(lines, strings.Join(meta, "  "))
		}

		t := "开始 " + a.StartsAt.In(loc).Format(timeLayout)
		if a.Status == "resolved" && !a.EndsAt.IsZero() {
			t += "，恢复 " + a.EndsAt.In(loc).Format(timeLayout) +
				"，持续 " + a.EndsAt.Sub(a.StartsAt).Round(time.Second).String()
		}
		lines = append(lines, t)

		var links []string
		if u := a.Annotations["runbook_url"]; u != "" {
			links = append(links, "[处理手册]("+u+")")
		}
		if a.GeneratorURL != "" {
			links = append(links, "[Prometheus]("+a.GeneratorURL+")")
		}
		if a.Status != "resolved" && externalURL != "" {
			links = append(links, "[静默]("+silenceURL(externalURL, a.Labels)+")")
		}
		if len(links) > 0 {
			lines = append(lines, strings.Join(links, " | "))
		}
	}
	return lines
}

// silenceURL 生成预填好匹配条件的 Alertmanager「新建静默」链接。
func silenceURL(base string, labels map[string]string) string {
	var ms []string
	for _, k := range []string{"alertname", "service", "instance", "route"} {
		if v := labels[k]; v != "" {
			ms = append(ms, fmt.Sprintf("%s=%q", k, v))
		}
	}
	return strings.TrimRight(base, "/") + "/#/silences/new?filter=" +
		url.QueryEscape("{"+strings.Join(ms, ",")+"}")
}
