package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestDisabled(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "TRUE": true, " true ": true, "false": false, "": false, "1": false} {
		if got := Disabled(func(string) string { return v }); got != want {
			t.Errorf("Disabled(%q) = %v", v, got)
		}
	}
}

func TestSetupDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	before := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatal("a provider is installed")
	}
}

func TestSetupEnabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	tp, mp := otel.GetTracerProvider(), otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetTracerProvider(tp); otel.SetMeterProvider(mp) })

	shutdown, err := Setup(context.Background(), "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatal("no SDK tracer provider")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandler(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(Handler{slog.NewJSONHandler(&buf, nil)})
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	log.InfoContext(ctx, "inside")
	span.End()
	log.Info("outside")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	var inside, outside map[string]any
	if err := json.Unmarshal(lines[0], &inside); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &outside); err != nil {
		t.Fatal(err)
	}
	if inside["trace_id"] != span.SpanContext().TraceID().String() || inside["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("inside %v", inside)
	}
	if _, ok := outside["trace_id"]; ok {
		t.Fatalf("outside %v", outside)
	}
}
