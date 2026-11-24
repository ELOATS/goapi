package telemetry

import (
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
)

// NATSHeaderCarrier 让 nats.Header 可以用来注入和提取链路上下文。
//
// 不能直接用 propagation.HeaderCarrier：它是给 HTTP 用的，读写时会把键规范化成 "Traceparent"。
// NATS 的消息头区分大小写，其它语言的客户端按 W3C 规范写的是小写 "traceparent"，
// 用 HeaderCarrier 去读就找不到，链路在消费方断开。
//
// 这里写入时保留传入的键（TraceContext 传的就是小写 traceparent），读取时不区分大小写，
// 所以老版本写入的 "Traceparent" 和别的客户端写入的 "traceparent" 都能读到。
type NATSHeaderCarrier nats.Header

var _ propagation.TextMapCarrier = NATSHeaderCarrier(nil)

func (c NATSHeaderCarrier) Get(key string) string {
	if vs := c[key]; len(vs) > 0 {
		return vs[0]
	}
	for k, vs := range c {
		if len(vs) > 0 && strings.EqualFold(k, key) {
			return vs[0]
		}
	}
	return ""
}

func (c NATSHeaderCarrier) Set(key, value string) {
	// 先删掉大小写不同的同名键，避免同一个头出现两份
	for k := range c {
		if k != key && strings.EqualFold(k, key) {
			delete(c, k)
		}
	}
	c[key] = []string{value}
}

func (c NATSHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
