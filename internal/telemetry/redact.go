package telemetry

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// redactingExporter wraps a delegate SpanExporter and strips the Discord
// webhook token from any recorded request-URL span attribute before spans
// reach the delegate. otelhttp sets the full request URL -- including path
// -- as a span attribute via span.SetAttributes after the span starts (not
// as a start-time attribute), so redaction has to happen at export time,
// once every attribute otelhttp will ever add is already present. For
// outbound Discord webhook requests that URL's path carries the delivery
// token (https://discord.com/api/webhooks/<id>/<token>), which must never
// reach a trace backend.
type redactingExporter struct {
	next sdktrace.SpanExporter
}

func newRedactingExporter(next sdktrace.SpanExporter) *redactingExporter {
	return &redactingExporter{next: next}
}

func (e *redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	redacted := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		redacted[i] = redactedSpan{s}
	}
	return e.next.ExportSpans(ctx, redacted)
}

func (e *redactingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// redactedSpan overrides Attributes() to redact webhook tokens; every other
// method is the embedded ReadOnlySpan's.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	attrs := s.ReadOnlySpan.Attributes()
	out := make([]attribute.KeyValue, len(attrs))
	copy(out, attrs)
	for i, attr := range out {
		// Don't gate on a fixed set of attribute keys (e.g. "url.full") --
		// a future otelhttp/semconv version could start recording the
		// request URL under a different key, and a key-based allowlist
		// would silently stop redacting it. Instead, inspect every
		// string-valued attribute: redactWebhookURL is a safe no-op on
		// anything that isn't a Discord webhook URL.
		if attr.Value.Type() != attribute.STRING {
			continue
		}
		if redacted, changed := redactWebhookURL(attr.Value.AsString()); changed {
			out[i] = attribute.String(string(attr.Key), redacted)
		}
	}
	return out
}

// redactWebhookURL replaces the token segment of a Discord webhook URL
// (.../api/webhooks/<id>/<token>[/...]) with a fixed placeholder. Returns
// the input unchanged (changed=false) if it doesn't look like a Discord
// webhook URL.
func redactWebhookURL(raw string) (redacted string, changed bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	// [... "api" "webhooks" <id> <token> ...]
	idx := -1
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "webhooks" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+2 >= len(segments) {
		return raw, false
	}

	segments[idx+2] = "REDACTED"
	u.Path = "/" + strings.Join(segments, "/")
	return u.String(), true
}
