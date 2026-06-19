package rss

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxFeedResponseBytes bounds how much of a feed response we will read into
// memory, to protect the controller from oversized or compression-bomb
// responses returned by a feed URL (which is fully operator-namespace
// controlled).
const maxFeedResponseBytes = 10 * 1024 * 1024 // 10MB

// defaultTimeout bounds how long a single feed fetch may take, since the
// feed URL is untrusted input and a slow/hanging server must not stall
// reconciliation indefinitely.
const defaultTimeout = 15 * time.Second

type Entry struct {
	ID          string
	Title       string
	Link        string
	Description string
	Published   time.Time
}

type Client struct {
	httpClient *http.Client
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = newDefaultHTTPClient()
	}

	return &Client{httpClient: httpClient}
}

// newDefaultHTTPClient returns a client with an explicit timeout and a
// DialContext that rejects connections to loopback, link-local, private, and
// other non-public IP ranges. Feed URLs are supplied by namespace users via
// the FeedGroup CRD, so without this guard the controller can be used as an
// SSRF proxy into the cluster network or cloud metadata endpoints.
func newDefaultHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !isPublicIP(ip) {
					return nil, fmt.Errorf("refusing to connect to non-public address %s", ip)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		ResponseHeaderTimeout: defaultTimeout,
	}

	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: transport,
	}
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() {
		return false
	}
	return true
}

func (c *Client) FetchEntries(ctx context.Context, feedURL string) ([]Entry, error) {
	if strings.TrimSpace(feedURL) == "" {
		return nil, fmt.Errorf("feed URL is empty")
	}

	parsedURL, err := url.ParseRequestURI(feedURL)
	if err != nil {
		return nil, fmt.Errorf("invalid feed URL %q: %w", feedURL, err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported feed URL scheme %q: only http/https are allowed", parsedURL.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("feed fetch failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	limitedBody := io.LimitReader(resp.Body, maxFeedResponseBytes+1)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, err
	}
	if len(data) > maxFeedResponseBytes {
		return nil, fmt.Errorf("feed response exceeds maximum allowed size of %d bytes", maxFeedResponseBytes)
	}

	entries, err := parseFeed(data)
	if err != nil {
		return nil, err
	}

	return entries, nil
}

type rssEnvelope struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
}

type atomEntry struct {
	ID        string   `xml:"id"`
	Title     string   `xml:"title"`
	Link      atomLink `xml:"link"`
	Summary   string   `xml:"summary"`
	Content   string   `xml:"content"`
	Updated   string   `xml:"updated"`
	Published string   `xml:"published"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

func parseFeed(data []byte) ([]Entry, error) {
	var envelope struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}

	switch strings.ToLower(envelope.XMLName.Local) {
	case "rss":
		return parseRSS(data)
	case "feed":
		return parseAtom(data)
	default:
		// Attempt RSS first, then Atom.
		entries, err := parseRSS(data)
		if err == nil {
			return entries, nil
		}
		return parseAtom(data)
	}
}

func parseRSS(data []byte) ([]Entry, error) {
	var envelope rssEnvelope
	if err := xml.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(envelope.Channel.Items))
	for _, item := range envelope.Channel.Items {
		published, _ := parseTime(item.PubDate)
		id := strings.TrimSpace(item.GUID)
		if id == "" {
			id = strings.TrimSpace(item.Link)
		}
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(item.Link),
			Description: strings.TrimSpace(item.Description),
			Published:   published,
		})
	}

	return entries, nil
}

func parseAtom(data []byte) ([]Entry, error) {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(feed.Entries))
	for _, item := range feed.Entries {
		published, _ := parseTime(item.Published)
		if published.IsZero() {
			published, _ = parseTime(item.Updated)
		}

		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(item.Link.Href)
		}
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}

		description := strings.TrimSpace(item.Summary)
		if description == "" {
			description = strings.TrimSpace(item.Content)
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(item.Link.Href),
			Description: description,
			Published:   published,
		})
	}

	return entries, nil
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}

	layouts := []string{
		time.RFC3339,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.ANSIC,
		"Mon, 2 Jan 2006 15:04:05 MST",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}
