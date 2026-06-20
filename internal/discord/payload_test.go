package discord

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fiveRunes is reused across the truncateRunes test cases below.
const fiveRunes = "abcde"

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty defaults to 1s", value: "", want: time.Second},
		{name: "whitespace defaults to 1s", value: "  ", want: time.Second},
		{name: "integer seconds", value: "5", want: 5 * time.Second},
		{name: "fractional seconds", value: "0.5", want: 500 * time.Millisecond},
		{name: "malformed defaults to 1s", value: "soon", want: time.Second},
		{name: "negative defaults to 1s", value: "-3", want: time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestRateLimitError_Error(t *testing.T) {
	err := &RateLimitError{RetryAfter: 3 * time.Second}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "3s") {
		t.Fatalf("unexpected error string: %q", err.Error())
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name  string
		runes string
		max   int
		want  string
	}{
		{name: "under limit unchanged", runes: fiveRunes, max: 10, want: fiveRunes},
		{name: "exactly at limit unchanged", runes: fiveRunes, max: 5, want: fiveRunes},
		{name: "zero max yields empty", runes: fiveRunes, max: 0, want: ""},
		{name: "negative max yields empty", runes: fiveRunes, max: -1, want: ""},
		{name: "truncates with ellipsis", runes: "hello world", max: 5, want: "hell…"},
		// The ellipsis is multi-byte but a single rune, so a multibyte body is
		// trimmed by rune count, not byte count.
		{name: "multibyte body trimmed by rune", runes: "日本語テスト", max: 3, want: "日本…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes([]rune(tc.runes), tc.max); got != tc.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tc.runes, tc.max, got, tc.want)
			}
		})
	}
}

func TestSendMessage_EmptyWebhookURL(t *testing.T) {
	c := NewClient("")
	err := c.SendMessageText(context.Background(), "empty webhook test")
	if err == nil || !strings.Contains(err.Error(), "webhook URL is empty") {
		t.Fatalf("expected empty-URL error, got %v", err)
	}
}

func TestSendMessage_EmptyContentAndEmbed(t *testing.T) {
	c := NewClient("https://discord.com/api/webhooks/1/abc")
	err := c.SendMessage(context.Background(), Message{})
	if err == nil || !strings.Contains(err.Error(), "content is empty") {
		t.Fatalf("expected empty-content error, got %v", err)
	}
}

func TestSendMessage_InvalidWebhookURL(t *testing.T) {
	c := NewClient("://not a url")
	err := c.SendMessageText(context.Background(), "invalid url test")
	if err == nil {
		t.Fatal("expected error for malformed webhook URL, got nil")
	}
}

func TestSendMessage_ReturnsRateLimitError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessageText(context.Background(), "rate limited test")

	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 2*time.Second {
		t.Fatalf("expected RetryAfter 2s, got %v", rle.RetryAfter)
	}
}

func TestSendMessage_ReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessageText(context.Background(), "non-2xx test")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
}

func TestEmbedToPayload_OmitsUnsetMedia(t *testing.T) {
	// An embed with only a title should produce no image/thumbnail/author/
	// footer sub-objects, so they stay omitted from the JSON.
	p := embedToPayload(Embed{Title: "only title"})
	if p.Image != nil || p.Thumbnail != nil || p.Author != nil || p.Footer != nil {
		t.Fatalf("expected unset embed media to be nil, got %+v", p)
	}
	if p.Title != "only title" {
		t.Fatalf("expected title preserved, got %q", p.Title)
	}
}

func TestClampEmbedTotalLength_UnderLimitUnchanged(t *testing.T) {
	e := Embed{Title: "t", Description: "d", FooterText: "f", AuthorName: "a"}
	got := clampEmbedTotalLength(e)
	if got.Description != "d" {
		t.Fatalf("expected description unchanged under limit, got %q", got.Description)
	}
}
