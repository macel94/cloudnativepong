package telemetry

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSetupWithoutEndpointIsNoopAndSafe(t *testing.T) {
	provider, err := Setup(context.Background(), func(string) string { return "" })
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	ctx, span := provider.Start(context.Background(), "test.noop", attribute.String("route", "/health"))
	if ctx == nil || span == nil {
		t.Fatal("no-op tracing returned nil context/span")
	}
	span.End()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestHTTPRouteBoundsDynamicPaths(t *testing.T) {
	tests := map[string]string{
		"/health":                        "/health",
		"/api/rooms/create":              "/api/rooms/create",
		"/rooms/a1b2c3/ws":               "/rooms/:room/ws",
		"/room/a1b2c3/ws":                "/room/:room/ws",
		"/internal/rooms/a1b2c3/started": "/internal/rooms/:room/callback",
		"/anything/with/private-data":    "unmatched",
	}
	for path, want := range tests {
		if got := HTTPRoute(path); got != want {
			t.Fatalf("HTTPRoute(%q) = %q, want %q", path, got, want)
		}
		if strings.Contains(HTTPRoute(path), "a1b2c3") {
			t.Fatalf("HTTPRoute(%q) leaked dynamic room data", path)
		}
	}
}

func TestW3CPropagationRoundTrip(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	defer otel.SetTracerProvider(old)

	ctx, span := otel.Tracer(instrumentationName).Start(context.Background(), "parent")
	headers := http.Header{}
	Inject(ctx, headers)
	if headers.Get("Traceparent") == "" {
		t.Fatal("Inject() did not set traceparent")
	}
	childCtx := Extract(context.Background(), headers)
	_, childSpan := otel.Tracer(instrumentationName).Start(childCtx, "child")
	childSpan.End()
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want 2", len(spans))
	}
	var parent, child *tracetest.SpanStub
	for i := range spans {
		switch spans[i].Name {
		case "parent":
			parent = &spans[i]
		case "child":
			child = &spans[i]
		}
	}
	if parent == nil || child == nil || !child.Parent.IsValid() || child.Parent.TraceID() != parent.SpanContext.TraceID() {
		t.Fatalf("child span did not preserve parent trace context: %+v", spans)
	}
}
