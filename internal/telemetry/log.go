package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceLogHandler 给每条带 ctx 的日志自动加上 trace_id / span_id，
// 这样在 Grafana / Jaeger 里可以从日志直接跳到对应的链路。
// 注意：必须用 logger.InfoContext(ctx, ...) 这类带 ctx 的方法才会生效。
type traceLogHandler struct {
	slog.Handler
}

func NewTraceLogHandler(h slog.Handler) slog.Handler {
	return traceLogHandler{Handler: h}
}

func (h traceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceLogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceLogHandler) WithGroup(name string) slog.Handler {
	return traceLogHandler{Handler: h.Handler.WithGroup(name)}
}
