package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestTraceLogHandlerAddsIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewTraceLogHandler(slog.NewJSONHandler(&buf, nil))).With("svc", "test")

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled,
	}))

	logger.InfoContext(ctx, "hello")
	out := buf.String()
	for _, want := range []string{`"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`, `"span_id":"00f067aa0ba902b7"`, `"svc":"test"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %s missing %s", out, want)
		}
	}

	buf.Reset()
	logger.InfoContext(context.Background(), "no span")
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("unexpected trace_id without span: %s", buf.String())
	}
}

func TestNilHTTPMetricsIsSafe(_ *testing.T) {
	var m *HTTPMetrics
	m.Begin()
	m.End("GET", "/x", 200, 0)
}
