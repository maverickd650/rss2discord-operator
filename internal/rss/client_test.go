package rss

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sampleRSS = `<?xml version="1.0"?>
<rss><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid></item>
</channel></rss>`

func TestFetchEntries_RejectsNonHTTPScheme(t *testing.T) {
	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(context.Background(), "ftp://example.com/feed.xml")
	if err == nil {
		t.Fatal("expected error for non-http(s) scheme, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported feed URL scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchEntries_EnforcesSizeCap(t *testing.T) {
	oversized := strings.Repeat("a", maxFeedResponseBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	defer srv.Close()

	// Use a plain client so loopback test servers aren't rejected by the
	// SSRF guard, isolating this test to the size-cap behavior.
	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchEntries_ParsesValidRSS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	entries, err := c.FetchEntries(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Title != "Hello" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestFetchEntries_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{Timeout: 10 * time.Millisecond})
	_, err := c.FetchEntries(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestDefaultClient_RejectsLoopbackTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	// NewClient(nil) builds the SSRF-guarded default client, which must
	// refuse to connect to a loopback address such as a local test server
	// or a feed URL pointing at 127.0.0.1.
	c := NewClient(nil)
	_, err := c.FetchEntries(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error connecting to loopback address, got nil")
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"169.254.169.254", false}, // cloud metadata endpoint
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"fe80::1", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("failed to parse test IP %q", tc.ip)
		}
		if got := isPublicIP(ip); got != tc.public {
			t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}
