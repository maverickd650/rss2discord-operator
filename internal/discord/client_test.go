package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendMessage_RejectsNonDiscordHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("attacker-controlled server should never receive a request")
	}))
	defer srv.Close()

	c := NewClient(srv.URL) // httptest URL is http://127.0.0.1:PORT, not discord.com
	err := c.SendMessage(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for non-Discord webhook host, got nil")
	}
	if !strings.Contains(err.Error(), "discord.com") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendMessage_RejectsHTTPScheme(t *testing.T) {
	c := NewClient("http://discord.com/api/webhooks/123/abc")
	err := c.SendMessage(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error for non-https scheme, got nil")
	}
}

func TestSendMessage_AcceptsDiscordHost(t *testing.T) {
	requestReceived := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// NewClientWithHTTP lets the test supply a client trusting the
	// httptest TLS cert. The httptest server resolves to 127.0.0.1, so we
	// temporarily add it to the allow-list to exercise the "happy path"
	// without depending on a real discord.com endpoint.
	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	if err := c.SendMessage(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !requestReceived {
		t.Fatal("expected request to reach the test server")
	}
}

func TestSendMessage_RespectsTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	client := srv.Client()
	client.Timeout = 10 * time.Millisecond
	c := NewClientWithHTTP(srv.URL, client)
	err := c.SendMessage(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected timeout-related error, got nil")
	}
}
