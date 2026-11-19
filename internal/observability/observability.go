// Package observability dựng log có cấu trúc và OpenTelemetry tracing.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup cấu hình logger mặc định và tracer provider toàn cục.
//
// Trace chỉ được xuất khi có OTEL_EXPORTER_OTLP_ENDPOINT hoặc
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT; nếu không, span vẫn được tạo (để có
// trace_id trong log và phản hồi lỗi) nhưng không gửi đi đâu.
//
// Hàm shutdown trả về phải được gọi khi process dừng để đẩy span còn lại.
func Setup(ctx context.Context, service, env string) (shutdown func(context.Context) error, err error) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)).
		With("service", service, "env", env))

	res := resource.NewSchemaless(
		attribute.String("service.name", service),
		attribute.String("deployment.environment.name", env),
	)
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	return tp.Shutdown, nil
}
