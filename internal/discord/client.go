package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds how long a single webhook delivery may take.
const defaultTimeout = 10 * time.Second

// AllowedWebhookHosts restricts message delivery to Discord's own webhook
// hosts. The webhook URL is read from a Secret the operator can `get`
// cluster-wide, so without this guard a misconfigured or malicious secret
// could redirect message content (and implicitly, an SSRF-style POST) to an
// arbitrary host. Exported so tests can register a mock server's host.
var AllowedWebhookHosts = map[string]bool{
	"discord.com":    true,
	"discordapp.com": true,
}

type Client struct {
	webhookURL string
	httpClient *http.Client
}

func NewClient(webhookURL string) *Client {
	return NewClientWithHTTP(webhookURL, nil)
}

func NewClientWithHTTP(webhookURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return &Client{webhookURL: strings.TrimSpace(webhookURL), httpClient: httpClient}
}

// NewHTTPClient returns the default HTTP client used for webhook delivery.
// Callers that build many *Client instances (one per FeedGroup webhook
// secret) should construct this once and share it via NewClientWithHTTP, so
// that connections to discord.com are pooled instead of rebuilt per client.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: defaultTimeout}
}

// Embed describes a Discord embed ("bubble") to attach to a webhook message,
// matching the subset of Discord's embed object that feeds need: a colored
// side bar, optional thumbnail/image, author line, and footer.
type Embed struct {
	Title       string
	Description string
	URL         string
	// Color is a 24-bit RGB integer (0xRRGGBB), as used by Discord's API.
	Color        int
	Timestamp    string
	ThumbnailURL string
	AuthorName   string
	FooterText   string
}

// Message is a single Discord webhook delivery. Content is sent as a plain
// message; Embed, if set, renders as a colored bubble alongside it. Setting
// ThreadName posts the message as a new thread in a forum channel; ThreadID
// posts into an existing thread/forum post instead.
type Message struct {
	Content    string
	Embed      *Embed
	ThreadName string
	ThreadID   string
	Username   string
	AvatarURL  string
}

type discordPayload struct {
	Content         string                 `json:"content,omitempty"`
	Embeds          []discordEmbed         `json:"embeds,omitempty"`
	ThreadName      string                 `json:"thread_name,omitempty"`
	Username        string                 `json:"username,omitempty"`
	AvatarURL       string                 `json:"avatar_url,omitempty"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

// discordAllowedMentions with an empty Parse list suppresses all @everyone,
// @here, role, and user mentions. Feed entry titles/descriptions are
// untrusted external text that ends up in the plain-content path, so
// without this a feed containing "@everyone" would actually ping the
// channel. Embeds are unaffected either way: Discord never parses mentions
// out of embed fields.
type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	URL         string              `json:"url,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Thumbnail   *discordEmbedMedia  `json:"thumbnail,omitempty"`
	Author      *discordEmbedAuthor `json:"author,omitempty"`
	Footer      *discordEmbedFooter `json:"footer,omitempty"`
}

type discordEmbedMedia struct {
	URL string `json:"url"`
}

type discordEmbedAuthor struct {
	Name string `json:"name"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

// RateLimitError indicates Discord responded with HTTP 429 and how long the
// caller must wait before retrying, per the response's Retry-After header.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("discord webhook rate limited, retry after %s", e.RetryAfter)
}

// parseRetryAfter parses Discord's Retry-After header, which is a number of
// seconds (optionally fractional). It defaults to 1s if the header is
// missing or malformed, so callers always back off at least briefly.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Second
	}

	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds < 0 {
		return time.Second
	}

	return time.Duration(seconds * float64(time.Second))
}

// SendMessage delivers msg to the webhook. Either Content or Embed (or both)
// must be set. SendMessageText is a convenience wrapper for the common
// plain-content-only case.
func (c *Client) SendMessage(ctx context.Context, msg Message) error {
	if strings.TrimSpace(c.webhookURL) == "" {
		return fmt.Errorf("discord webhook URL is empty")
	}
	if strings.TrimSpace(msg.Content) == "" && msg.Embed == nil {
		return fmt.Errorf("discord message content is empty")
	}

	parsedURL, err := url.ParseRequestURI(c.webhookURL)
	if err != nil {
		return fmt.Errorf("invalid discord webhook URL: %w", err)
	}
	if parsedURL.Scheme != "https" || !AllowedWebhookHosts[strings.ToLower(parsedURL.Hostname())] {
		return fmt.Errorf("discord webhook URL must be an https://discord.com or https://discordapp.com URL")
	}

	if strings.TrimSpace(msg.ThreadID) != "" {
		query := parsedURL.Query()
		query.Set("thread_id", msg.ThreadID)
		parsedURL.RawQuery = query.Encode()
	}

	payload := discordPayload{
		Content:         msg.Content,
		ThreadName:      msg.ThreadName,
		Username:        msg.Username,
		AvatarURL:       msg.AvatarURL,
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}
	if msg.Embed != nil {
		payload.Embeds = []discordEmbed{embedToPayload(*msg.Embed)}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedURL.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var responseBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&responseBody)
		return fmt.Errorf("discord webhook returned %s", resp.Status)
	}

	return nil
}

// SendMessageText is a convenience wrapper around SendMessage for plain,
// embed-less content.
func (c *Client) SendMessageText(ctx context.Context, content string) error {
	return c.SendMessage(ctx, Message{Content: content})
}

// maxEmbedTotalLength is Discord's cap on the combined character count of
// title + description + footer.text + author.name (+ field name/value, which
// this package doesn't use) across all embeds in a single message. Discord
// rejects the whole webhook call with a 400 if exceeded, even though each
// field individually stays under its own per-field limit.
const maxEmbedTotalLength = 6000

func embedToPayload(e Embed) discordEmbed {
	e = clampEmbedTotalLength(e)
	payload := discordEmbed{
		Title:       e.Title,
		Description: e.Description,
		URL:         e.URL,
		Color:       e.Color,
		Timestamp:   e.Timestamp,
	}
	if e.ThumbnailURL != "" {
		payload.Thumbnail = &discordEmbedMedia{URL: e.ThumbnailURL}
	}
	if e.AuthorName != "" {
		payload.Author = &discordEmbedAuthor{Name: e.AuthorName}
	}
	if e.FooterText != "" {
		payload.Footer = &discordEmbedFooter{Text: e.FooterText}
	}
	return payload
}

// clampEmbedTotalLength shrinks Description, the field with the most slack,
// until title+description+footer+author fits within maxEmbedTotalLength.
// Title/footer/author come from feed config and entry titles, which are
// short in practice, so trimming the (often long) description is the least
// surprising way to enforce the limit.
func clampEmbedTotalLength(e Embed) Embed {
	total := len([]rune(e.Title)) + len([]rune(e.Description)) + len([]rune(e.FooterText)) + len([]rune(e.AuthorName))
	if total <= maxEmbedTotalLength {
		return e
	}

	overflow := total - maxEmbedTotalLength
	descRunes := []rune(e.Description)
	newLen := max(len(descRunes)-overflow, 0)
	e.Description = truncateRunes(descRunes, newLen)
	return e
}

// truncateRunes trims runes to at most max, appending an ellipsis in place
// of the last truncated rune when anything was actually cut.
func truncateRunes(runes []rune, max int) string {
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 0 {
		return ""
	}
	const ellipsis = "…"
	return string(runes[:max-len([]rune(ellipsis))]) + ellipsis
}
