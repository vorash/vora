package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestSetupTracingDisabledIsNoop(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), TracingConfig{})
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if id := TraceID(context.Background()); id != "" {
		t.Fatalf("TraceID with tracing off = %q, want empty", id)
	}
}

func TestContextHandlerAddsTraceAndSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01},
		SpanID:     trace.SpanID{0x02},
		TraceFlags: trace.FlagsSampled,
	})
	logger.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "hello")

	out := buf.String()
	if !strings.Contains(out, "trace_id") || !strings.Contains(out, "span_id") {
		t.Fatalf("log line missing trace ids: %s", out)
	}
	if TraceID(context.Background()) != "" {
		t.Fatal("a bare context must have no trace id")
	}
}

func TestContextHandlerWithoutSpanOmitsIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	logger.Info("hello")
	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("log line must not carry a trace id: %s", buf.String())
	}
}
