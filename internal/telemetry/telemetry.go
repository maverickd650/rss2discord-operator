// Package telemetry wires up OpenTelemetry tracing for the operator's
// outbound HTTP paths (RSS fetch, Discord webhook delivery). It is inert
// unless Setup is called with a non-empty endpoint.
package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "rss2discord-operator"

// Setup builds a TracerProvider that exports spans to endpoint over
// OTLP/gRPC and registers it as the global provider. Every span is passed
// through a redacting processor (see redactingProcessor) that strips webhook
// tokens from any recorded request URL before export. The returned shutdown
// func flushes and closes the exporter; callers should invoke it on process
// shutdown (e.g. via manager.Add).
func Setup(ctx context.Context, endpoint string) (trace.TracerProvider, func(context.Context) error, error) {
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, nil, fmt.Errorf("building trace resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(newRedactingExporter(exporter)),
	)
	otel.SetTracerProvider(tp)

	return tp, tp.Shutdown, nil
}

// HTTPTransportWrap returns a wrap func suitable for
// rss.NewClientWithTransportWrap / discord.NewHTTPClientWithTransportWrap:
// it instruments the given base transport with otelhttp using tp, without
// replacing it. Callers must wrap the SSRF-guarded transport, never bypass
// it.
func HTTPTransportWrap(tp trace.TracerProvider) func(http.RoundTripper) http.RoundTripper {
	return func(base http.RoundTripper) http.RoundTripper {
		return otelhttp.NewTransport(base, otelhttp.WithTracerProvider(tp))
	}
}
