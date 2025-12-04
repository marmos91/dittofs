package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// tracer is the global tracer
	tracer     trace.Tracer
	tracerOnce sync.Once

	// tracerProvider is the global tracer provider
	tracerProvider *sdktrace.TracerProvider

	// enabled indicates whether telemetry is enabled
	enabled bool
)

// Init initializes the OpenTelemetry SDK with the given configuration.
// Returns a shutdown function that should be called to flush and close the exporter.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		// When disabled, use a no-op tracer
		enabled = false
		tracer = noop.NewTracerProvider().Tracer("dittofs")
		return func(context.Context) error { return nil }, nil
	}

	enabled = true

	// Create OTLP exporter
	var opts []otlptracegrpc.Option
	opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))

	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
		resource.WithHost(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create sampler based on sample rate
	var sampler sdktrace.Sampler
	if cfg.SampleRate >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRate <= 0.0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	// Create tracer provider
	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set global tracer provider
	otel.SetTracerProvider(tracerProvider)

	// Set global propagator for trace context propagation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Get the tracer
	tracer = tracerProvider.Tracer(cfg.ServiceName)

	// Return shutdown function
	shutdown = func(ctx context.Context) error {
		// Use a timeout context for shutdown
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tracerProvider.Shutdown(shutdownCtx)
	}

	return shutdown, nil
}

// Tracer returns the global tracer for creating spans.
// If telemetry is not initialized, returns a no-op tracer.
func Tracer() trace.Tracer {
	tracerOnce.Do(func() {
		if tracer == nil {
			tracer = noop.NewTracerProvider().Tracer("dittofs")
		}
	})
	return tracer
}

// IsEnabled returns whether telemetry is enabled
func IsEnabled() bool {
	return enabled
}

// StartSpan starts a new span with the given name.
// Returns the new context (with span) and the span.
// The caller must call span.End() when done.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// SpanFromContext returns the current span from the context.
// If there is no span in the context, returns a no-op span.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// AddEvent adds an event to the current span in the context.
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent(name, trace.WithAttributes(attrs...))
}

// RecordError records an error on the current span.
// This also sets the span status to Error.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// SetStatus sets the status of the current span.
func SetStatus(ctx context.Context, code codes.Code, description string) {
	span := trace.SpanFromContext(ctx)
	span.SetStatus(code, description)
}

// SetAttributes sets attributes on the current span.
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attrs...)
}

// TraceID returns the trace ID from the current span context.
// Returns empty string if no span is active.
func TraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// SpanID returns the span ID from the current span context.
// Returns empty string if no span is active.
func SpanID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	if sc.HasSpanID() {
		return sc.SpanID().String()
	}
	return ""
}
