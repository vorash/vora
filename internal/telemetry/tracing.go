package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/vorash/vora/internal/version"
)

// TracingConfig configures distributed tracing. With an empty Endpoint,
// tracing is disabled: the global provider stays the default no-op, so spans
// cost nothing and no exporter is created.
type TracingConfig struct {
	// Endpoint is the OTLP gRPC collector address (host:port). Empty disables
	// export. Typically OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string
	// ServiceName is the resource service.name.
	ServiceName string
	// Insecure uses plaintext gRPC to the collector (no TLS).
	Insecure bool
}

// SetupTracing installs the global tracer provider and W3C propagator, and
// returns a shutdown function. The propagator is set even when export is
// disabled, so trace context still flows between processes.
func SetupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	noop := func(context.Context) error { return nil }
	if cfg.Endpoint == "" {
		return noop, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "vora"
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("tracing: build OTLP exporter: %w", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", version.String()),
	))
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Tracer returns the tracer for the given instrumentation scope.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Start begins a span from ctx. With tracing disabled it is a no-op span.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer("vora").Start(ctx, name, trace.WithAttributes(attrs...))
}

// TraceID returns the trace id of the span in ctx, or "" when there is none.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// ContextHandler is a slog.Handler that adds the trace_id and span_id of the
// span in the record's context. Callers must use the *Context log methods
// (logger.InfoContext, etc.) for the ids to appear.
type ContextHandler struct{ slog.Handler }

// Handle implements slog.Handler.
func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{h.Handler.WithGroup(name)}
}
