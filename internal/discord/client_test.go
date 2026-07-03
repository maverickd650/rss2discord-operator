package discord

import (
	"encoding/json"
	"errors"
	"io"
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
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for non-Discord webhook host, got nil")
	}
	if !strings.Contains(err.Error(), "discord.com") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewHTTPClientWithTransportWrap_WrapsAndPreservesRedirectRefusal(t *testing.T) {
	var wrapped bool
	wrap := func(rt http.RoundTripper) http.RoundTripper {
		wrapped = true
		return rt
	}

	client := NewHTTPClientWithTransportWrap(wrap)
	if !wrapped {
		t.Fatal("expected wrap to be invoked when building the client")
	}
	if client.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect to still refuse redirects")
	}
}

func TestNewHTTPClientWithTransportWrap_NilWrapMatchesNewHTTPClient(t *testing.T) {
	client := NewHTTPClientWithTransportWrap(nil)
	if client.Transport == nil {
		t.Fatal("expected a non-nil transport")
	}
	if client.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect to still refuse redirects")
	}
}

func TestSendMessage_DoesNotFollowRedirect(t *testing.T) {
	attackerHit := false
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHit = true
	}))
	defer attacker.Close()

	// An allowlisted host redirecting to an arbitrary attacker-controlled
	// server. NewHTTPClient's CheckRedirect must stop http.Client from
	// transparently following it and replaying the message content there.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	client := srv.Client()
	client.CheckRedirect = NewHTTPClient().CheckRedirect

	c := NewClientWithHTTP(srv.URL, client)
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected the unfollowed redirect's 3xx status to surface as an error")
	}
	if attackerHit {
		t.Fatal("redirect target must never receive the request")
	}
}

func TestSendMessage_SharedRateLimiterCooldownSkipsRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	limiter := NewRateLimiter()
	// Two separate *Client instances for the same webhook, as
	// DiscordClientBuilder constructs one per FeedGroup reconcile -- sharing
	// a limiter is what makes one FeedGroup's 429 also hold off another.
	c1 := NewClientWithLimiter(srv.URL, srv.Client(), limiter)
	c2 := NewClientWithLimiter(srv.URL, srv.Client(), limiter)

	// First send actually hits the server and gets 429'd, which records the
	// cooldown.
	if _, ok := errors.AsType[*RateLimitError](c1.SendMessageText(t.Context(), "first")); !ok {
		t.Fatal("expected the first send to reach the server and return *RateLimitError")
	}
	if requests != 1 {
		t.Fatalf("expected exactly 1 request so far, got %d", requests)
	}

	// A second Client for the same webhook, sharing the limiter, must be
	// short-circuited by the cooldown rather than issuing another request.
	err := c2.SendMessageText(t.Context(), "second")
	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("expected *RateLimitError from the shared cooldown, got %v", err)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > 60*time.Second {
		t.Fatalf("expected RetryAfter in (0, 60s], got %v", rle.RetryAfter)
	}
	if requests != 1 {
		t.Fatalf("expected the second send to be short-circuited without a request, got %d requests", requests)
	}
}

func TestSendMessage_NilRateLimiterDoesNotCooldown(t *testing.T) {
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	// NewClientWithHTTP (no limiter) must behave exactly as before this
	// change: every send reaches the server, with no cross-Client cooldown.
	c := NewClientWithHTTP(srv.URL, srv.Client())
	_, _ = c.SendMessageText(t.Context(), "first"), c.SendMessageText(t.Context(), "second")
	if requests != 2 {
		t.Fatalf("expected both sends to reach the server without a shared limiter, got %d requests", requests)
	}
}

func TestSendMessage_RejectsHTTPScheme(t *testing.T) {
	c := NewClient("http://discord.com/api/webhooks/123/abc")
	err := c.SendMessageText(t.Context(), "hello")
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
	if err := c.SendMessageText(t.Context(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !requestReceived {
		t.Fatal("expected request to reach the test server")
	}
}

func TestSendMessage_SuppressesMentionsByDefault(t *testing.T) {
	var receivedBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessageText(t.Context(), "@everyone breaking news")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedBody, `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("expected allowed_mentions to suppress all mentions, got %q", receivedBody)
	}
}

func TestSendMessage_RedactsWebhookURLOnTransportError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not receive a request once closed")
	}))
	webhookURL := srv.URL + "/api/webhooks/123456789/SUPER-SECRET-TOKEN"
	srv.Close() // closing first forces a dial/connection-refused error below

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(webhookURL, srv.Client())
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("error leaked webhook token: %v", err)
	}
}

func TestSendMessage_ClampsCombinedEmbedLength(t *testing.T) {
	var receivedBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessage(t.Context(), Message{
		Embed: &Embed{
			Title:       strings.Repeat("t", 256),
			Description: strings.Repeat("d", 4096),
			FooterText:  strings.Repeat("f", 2048),
			AuthorName:  strings.Repeat("a", 256),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal([]byte(receivedBody), &payload); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(payload.Embeds))
	}
	embed := payload.Embeds[0]
	total := len([]rune(embed.Title)) + len([]rune(embed.Description)) + len([]rune(embed.Footer.Text)) + len([]rune(embed.Author.Name))
	if total > maxEmbedTotalLength {
		t.Fatalf("expected combined embed length <= %d, got %d", maxEmbedTotalLength, total)
	}
}

func TestSendMessage_SendsEmbedWithColorAndThumbnail(t *testing.T) {
	var receivedBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessage(t.Context(), Message{
		Embed: &Embed{
			Title:        "Breaking News",
			Description:  "Something happened",
			URL:          "https://example.com/article",
			Color:        0x00FF00,
			ThumbnailURL: "https://example.com/image.png",
			AuthorName:   "Example Feed",
			FooterText:   "example.com",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		`"title":"Breaking News"`,
		`"color":65280`,
		`"thumbnail":{"url":"https://example.com/image.png"}`,
		`"author":{"name":"Example Feed"}`,
		`"footer":{"text":"example.com"}`,
	} {
		if !strings.Contains(receivedBody, want) {
			t.Fatalf("expected request body to contain %q, got %q", want, receivedBody)
		}
	}
}

func TestSendMessage_SetsThreadNameForForumChannel(t *testing.T) {
	var receivedBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessage(t.Context(), Message{Content: "hello", ThreadName: "New Post Title"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedBody, `"thread_name":"New Post Title"`) {
		t.Fatalf("expected request body to contain thread_name, got %q", receivedBody)
	}
}

func TestSendMessage_SetsThreadIDQueryParamForExistingThread(t *testing.T) {
	var receivedQuery string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessage(t.Context(), Message{Content: "hello", ThreadID: "123456789"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedQuery != "thread_id=123456789" {
		t.Fatalf("expected thread_id query param, got %q", receivedQuery)
	}
}

func TestSendMessage_SurfacesDiscordErrorDetail(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Invalid Webhook Token","code":50027}`))
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
	for _, want := range []string{"400", "Invalid Webhook Token", "50027"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestSendMessage_ErrorWithoutBodyStillReturnsStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to contain status 500, got %q", err.Error())
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
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected timeout-related error, got nil")
	}
}
