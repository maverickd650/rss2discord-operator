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

const testETag = `"abc123"`

func TestFetchEntries_RejectsNonHTTPScheme(t *testing.T) {
	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(context.Background(), "ftp://example.com/feed.xml", CacheValidators{})
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
	_, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{})
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
	result, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Title != "Hello" {
		t.Fatalf("unexpected entries: %+v", result.Entries)
	}
}

func TestFetchEntries_SendsETagAndLastModifiedValidators(t *testing.T) {
	var gotIfNoneMatch, gotIfModifiedSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		gotIfModifiedSince = r.Header.Get("If-Modified-Since")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	_, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{
		ETag:         testETag,
		LastModified: "Wed, 21 Oct 2015 07:28:00 GMT",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIfNoneMatch != testETag {
		t.Fatalf("expected If-None-Match to be sent, got %q", gotIfNoneMatch)
	}
	if gotIfModifiedSince != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Fatalf("expected If-Modified-Since to be sent, got %q", gotIfModifiedSince)
	}
}

func TestFetchEntries_StoresValidatorsFromSuccessfulResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"new-etag"`)
		w.Header().Set("Last-Modified", "Thu, 22 Oct 2015 07:28:00 GMT")
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NotModified {
		t.Fatal("expected NotModified to be false on a 200 response")
	}
	if result.ETag != `"new-etag"` {
		t.Fatalf("expected ETag to be captured, got %q", result.ETag)
	}
	if result.LastModified != "Thu, 22 Oct 2015 07:28:00 GMT" {
		t.Fatalf("expected Last-Modified to be captured, got %q", result.LastModified)
	}
}

func TestFetchEntries_HandlesNotModifiedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == testETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Fatal("expected request to carry the previously stored ETag")
	}))
	defer srv.Close()

	c := NewClient(&http.Client{})
	result, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{ETag: testETag})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.NotModified {
		t.Fatal("expected NotModified to be true on a 304 response")
	}
	if len(result.Entries) != 0 {
		t.Fatalf("expected no entries on a 304 response, got %+v", result.Entries)
	}
	if result.ETag != testETag {
		t.Fatalf("expected the prior ETag to be preserved, got %q", result.ETag)
	}
}

func TestParseFeed_ExtractsRSSEnclosureImage(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<rss><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<enclosure url="http://example.com/pic.jpg" type="image/jpeg" /></item>
</channel></rss>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Image != "http://example.com/pic.jpg" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestParseFeed_ExtractsMediaThumbnailImage(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<media:thumbnail url="http://example.com/thumb.jpg" /></item>
</channel></rss>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Image != "http://example.com/thumb.jpg" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestParseFeed_ExtractsAtomEnclosureImage(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>1</id>
<title>Hello</title>
<link rel="alternate" href="http://example.com/1" />
<link rel="enclosure" href="http://example.com/pic.jpg" type="image/jpeg" />
<summary>World</summary>
</entry>
</feed>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].Link != "http://example.com/1" {
		t.Fatalf("unexpected link: %q", entries[0].Link)
	}
	if entries[0].Image != "http://example.com/pic.jpg" {
		t.Fatalf("unexpected image: %q", entries[0].Image)
	}
}

func TestFetchEntries_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	c := NewClient(&http.Client{Timeout: 10 * time.Millisecond})
	_, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{})
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
	_, err := c.FetchEntries(context.Background(), srv.URL, CacheValidators{})
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
		{"100.64.0.1", false},       // carrier-grade NAT (RFC 6598)
		{"100.127.255.254", false},  // top of the CGNAT block
		{"100.63.255.255", true},    // just below the CGNAT block
		{"::ffff:127.0.0.1", false}, // IPv4-mapped IPv6 loopback
		{"::ffff:10.0.0.1", false},  // IPv4-mapped IPv6 private
		{"::ffff:8.8.8.8", true},    // IPv4-mapped IPv6 public
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
