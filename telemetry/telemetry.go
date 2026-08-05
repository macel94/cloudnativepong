// Package telemetry provides optional OpenTelemetry tracing for Cloud Native Pong.
//
// Tracing is deliberately opt-in: without OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, the package uses the OpenTelemetry no-op
// provider and the application has no collector dependency at runtime.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/cloudnativepong"
	serviceName         = "cloudnativepong"
)

// Provider owns the process tracer provider. It is safe to use when tracing
// is disabled; spans are then no-op and Shutdown is harmless.
type Provider struct {
	tracer   trace.Tracer
	shutdown func(context.Context) error
}

// Setup configures the global W3C propagator and, when an OTLP endpoint is
// explicitly configured, an OTLP/HTTP batch exporter.
func Setup(ctx context.Context, getenv func(string) string) (*Provider, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	tracesEndpoint := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if endpoint == "" && tracesEndpoint == "" {
		return &Provider{
			tracer:   otel.Tracer(instrumentationName),
			shutdown: func(context.Context) error { return nil },
		}, nil
	}

	options := make([]otlptracehttp.Option, 0, 2)
	if tracesEndpoint != "" {
		options = append(options, otlptracehttp.WithEndpointURL(tracesEndpoint))
	} else if endpoint != "" {
		options = append(options, otlptracehttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/traces"))
	}
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE")), "true") || strings.EqualFold(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_INSECURE")), "true") {
		options = append(options, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			"",
			attribute.String("service.name", serviceName),
			attribute.String("service.version", "1.0.0"),
		)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	otel.SetTracerProvider(provider)

	return &Provider{
		tracer: provider.Tracer(instrumentationName),
		shutdown: func(shutdownCtx context.Context) error {
			return provider.Shutdown(shutdownCtx)
		},
	}, nil
}

// Start begins a span using the process provider and returns its child context.
func (p *Provider) Start(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	if p == nil || p.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return p.tracer.Start(ctx, name, trace.WithAttributes(attributes...))
}

// Shutdown flushes configured exporters. It is safe for the no-op provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Extract extracts W3C trace context from HTTP headers.
func Extract(ctx context.Context, headers http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(headers))
}

// Inject adds W3C trace context to HTTP/WebSocket headers.
func Inject(ctx context.Context, headers http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(headers))
}

// HTTPRoute normalizes dynamic paths into bounded route names. It deliberately
// does not return room IDs or arbitrary user-controlled path material.
func HTTPRoute(path string) string {
	switch {
	case path == "/health":
		return "/health"
	case path == "/metrics":
		return "/metrics"
	case path == "/api/rooms":
		return "/api/rooms"
	case path == "/api/rooms/create":
		return "/api/rooms/create"
	case path == "/api/rooms/join":
		return "/api/rooms/join"
	case strings.HasPrefix(path, "/rooms/"):
		return "/rooms/:room/ws"
	case strings.HasPrefix(path, "/room/"):
		return "/room/:room/ws"
	case strings.HasPrefix(path, "/internal/rooms/"):
		return "/internal/rooms/:room/callback"
	default:
		return "unmatched"
	}
}

// ShutdownContext is a bounded default used by process shutdown paths.
func ShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
