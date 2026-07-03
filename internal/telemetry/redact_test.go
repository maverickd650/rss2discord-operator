package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRedactWebhookURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{
			name:    "webhook URL with token",
			in:      "https://discord.com/api/webhooks/123456789/super-secret-token",
			want:    "https://discord.com/api/webhooks/123456789/REDACTED",
			changed: true,
		},
		{
			name:    "webhook URL with trailing path segment",
			in:      "https://discord.com/api/webhooks/123456789/super-secret-token/slack",
			want:    "https://discord.com/api/webhooks/123456789/REDACTED/slack",
			changed: true,
		},
		{
			name:    "unrelated URL untouched",
			in:      "https://news.ycombinator.com/rss",
			want:    "https://news.ycombinator.com/rss",
			changed: false,
		},
		{
			name:    "webhooks path with no token segment untouched",
			in:      "https://discord.com/api/webhooks/123456789",
			want:    "https://discord.com/api/webhooks/123456789",
			changed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := redactWebhookURL(tt.in)
			if changed != tt.changed {
				t.Errorf("changed = %v, want %v", changed, tt.changed)
			}
			if got != tt.want {
				t.Errorf("redactWebhookURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRedactingExporter_StripsTokenFromExportedSpan verifies the token
// redaction happens end-to-end: a real otelhttp-instrumented request to a
// URL carrying a Discord-shaped webhook token must not leak that token into
// an exported span's URL attribute.
func TestRedactingExporter_StripsTokenFromExportedSpan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(
		trace.WithSyncer(newRedactingExporter(exporter)),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithTracerProvider(tp)),
	}

	webhookURL := srv.URL + "/api/webhooks/123456789/super-secret-token"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, webhookURL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	_ = resp.Body.Close()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one exported span")
	}

	found := false
	for _, span := range spans {
		for _, attr := range span.Attributes {
			if attr.Key != "url.full" && attr.Key != "http.url" {
				continue
			}
			found = true
			if v := attr.Value.AsString(); strings.Contains(v, "super-secret-token") {
				t.Errorf("span attribute %s = %q still contains the webhook token", attr.Key, v)
			}
		}
	}
	if !found {
		t.Fatal("no span carried a url.full/http.url attribute; test didn't exercise redaction")
	}
}
