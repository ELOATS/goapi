// Package telemetry 负责链路追踪、指标和日志关联。
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type TracingConfig struct {
	Enabled     bool
	ServiceName string
	Env         string
	SampleRatio float64
}

// SetupTracing 安装全局 TracerProvider 和 W3C 传播器，返回的函数用于退出时刷新剩余 span。
//
// 导出器的地址、协议头、TLS 等全部由标准环境变量控制，例如
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
//	OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer xxx
//
// 未开启时仍会安装传播器：上游传来的 traceparent 会被透传到日志和下游。
func SetupTracing(ctx context.Context, c TracingConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !c.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	// 后面的选项会覆盖前面的：OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES 优先于代码默认值。
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", c.ServiceName),
			attribute.String("deployment.environment.name", c.Env),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
