package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{webhookURL: strings.TrimSpace(webhookURL), httpClient: httpClient}
}

type discordPayload struct {
	Content string `json:"content"`
}

func (c *Client) SendMessage(ctx context.Context, content string) error {
	if strings.TrimSpace(c.webhookURL) == "" {
		return fmt.Errorf("discord webhook URL is empty")
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("discord message content is empty")
	}

	parsedURL, err := url.ParseRequestURI(c.webhookURL)
	if err != nil {
		return fmt.Errorf("invalid discord webhook URL: %w", err)
	}
	if parsedURL.Scheme != "https" || !AllowedWebhookHosts[strings.ToLower(parsedURL.Hostname())] {
		return fmt.Errorf("discord webhook URL must be an https://discord.com or https://discordapp.com URL")
	}

	payload := discordPayload{Content: content}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, strings.NewReader(string(body)))
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

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var responseBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&responseBody)
		return fmt.Errorf("discord webhook returned %s", resp.Status)
	}

	return nil
}
