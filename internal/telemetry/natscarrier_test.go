package telemetry

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	testTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	testTraceparent = "00-" + testTraceID + "-00f067aa0ba902b7-01"
)

func TestNATSHeaderCarrierExtractIsCaseInsensitive(t *testing.T) {
	prop := propagation.TraceContext{}
	for _, key := range []string{"traceparent", "Traceparent", "TRACEPARENT"} {
		h := nats.Header{}
		h[key] = []string{testTraceparent} // 直接写 map，不经过任何规范化
		ctx := prop.Extract(context.Background(), NATSHeaderCarrier(h))
		if got := trace.SpanContextFromContext(ctx).TraceID().String(); got != testTraceID {
			t.Errorf("header key %q: trace id = %s, want %s", key, got, testTraceID)
		}
	}
}

func TestNATSHeaderCarrierRoundTrip(t *testing.T) {
	prop := propagation.TraceContext{}
	tid, _ := trace.TraceIDFromHex(testTraceID)
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))

	h := nats.Header{}
	h["Traceparent"] = []string{"00-ffffffffffffffffffffffffffffffff-ffffffffffffffff-01"} // 旧值，应被替换
	prop.Inject(ctx, NATSHeaderCarrier(h))

	if _, ok := h["traceparent"]; !ok {
		t.Fatalf("inject should write lowercase traceparent (W3C), got %v", h)
	}
	if _, ok := h["Traceparent"]; ok {
		t.Fatalf("differently-cased duplicate should be removed, got %v", h)
	}
	out := prop.Extract(context.Background(), NATSHeaderCarrier(h))
	if got := trace.SpanContextFromContext(out).TraceID(); got != tid {
		t.Fatalf("round trip trace id = %s, want %s", got, tid)
	}
}
