// Package telemetry sets up OpenTelemetry traces and metrics exported over
// OTLP, configured by the standard OTEL_* environment variables, adds trace
// and span IDs to slog records, and traces outgoing HTTP requests.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Disabled reports whether OTEL_SDK_DISABLED is true, which the Go SDK does
// not honour by itself.
func Disabled(getenv func(string) string) bool {
	return strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true")
}

// Setup installs global tracer and meter providers for the service, with
// export errors logged to log, and returns the function that flushes and
// stops them. When OTEL_SDK_DISABLED is true it installs nothing, creates no
// exporter and connects nowhere; the global providers stay no-ops.
// OTEL_SERVICE_NAME overrides service.
func Setup(ctx context.Context, service string, log *slog.Logger) (func(context.Context) error, error) {
	if Disabled(os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Warn("telemetry", "err", err) }))
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}
	spans, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, err
	}
	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, errors.Join(err, spans.Shutdown(ctx))
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(spans), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Flush calls the shutdown Setup returned, giving it a few seconds past the
// end of ctx to export what it holds, and logs its error.
func Flush(ctx context.Context, shutdown func(context.Context) error, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Warn("telemetry shutdown", "err", err)
	}
}

// Handler adds trace_id and span_id to records logged with a context that
// carries a span.
type Handler struct {
	slog.Handler
}

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return Handler{h.Handler.WithAttrs(attrs)}
}

func (h Handler) WithGroup(name string) slog.Handler {
	return Handler{h.Handler.WithGroup(name)}
}
