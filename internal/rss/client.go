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
	Image       string
	Author      string
	Categories  []string
	Published   time.Time
}

// CacheValidators carries the conditional-GET validators returned by a
// previous fetch of a feed, so a later fetch can ask the server "has this
// changed since I last asked" instead of always re-downloading and
// re-parsing the full feed body.
type CacheValidators struct {
	ETag         string
	LastModified string
}

// FetchResult is the outcome of a conditional fetch. When NotModified is
// true (the server responded 304), Entries is nil and the caller should
// keep using whatever it already has; ETag/LastModified are carried through
// unchanged in that case so the caller can keep sending them next time.
type FetchResult struct {
	Entries      []Entry
	NotModified  bool
	ETag         string
	LastModified string
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

// cgnatBlock is the shared address space carriers use for NAT
// (RFC 6598, 100.64.0.0/10). net.IP.IsPrivate doesn't cover it, but like
// RFC 1918 space it can route to internal infrastructure that a feed URL
// must not be able to reach.
var cgnatBlock = func() *net.IPNet {
	_, block, _ := net.ParseCIDR("100.64.0.0/10")
	return block
}()

func isPublicIP(ip net.IP) bool {
	// Unwrap IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) so the checks below
	// (several of which only special-case the 4-byte form) see the actual
	// IPv4 address instead of treating it as an opaque IPv6 literal.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() || cgnatBlock.Contains(ip) {
		return false
	}
	return true
}

// FetchEntries fetches and parses feedURL. If validators carries an ETag
// and/or LastModified from a previous fetch, they're sent as If-None-Match
// / If-Modified-Since so an unchanged feed costs a 304 instead of a full
// re-download and re-parse.
func (c *Client) FetchEntries(ctx context.Context, feedURL string, validators CacheValidators) (FetchResult, error) {
	if strings.TrimSpace(feedURL) == "" {
		return FetchResult{}, fmt.Errorf("feed URL is empty")
	}

	parsedURL, err := url.ParseRequestURI(feedURL)
	if err != nil {
		return FetchResult{}, fmt.Errorf("invalid feed URL %q: %w", feedURL, err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return FetchResult{}, fmt.Errorf("unsupported feed URL scheme %q: only http/https are allowed", parsedURL.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FetchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		result := FetchResult{NotModified: true, ETag: validators.ETag, LastModified: validators.LastModified}
		if etag := resp.Header.Get("ETag"); etag != "" {
			result.ETag = etag
		}
		if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" {
			result.LastModified = lastModified
		}
		return result, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return FetchResult{}, fmt.Errorf("feed fetch failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	limitedBody := io.LimitReader(resp.Body, maxFeedResponseBytes+1)
	data, err := io.ReadAll(limitedBody)
	if err != nil {
		return FetchResult{}, err
	}
	if len(data) > maxFeedResponseBytes {
		return FetchResult{}, fmt.Errorf("feed response exceeds maximum allowed size of %d bytes", maxFeedResponseBytes)
	}

	entries, err := parseFeed(data)
	if err != nil {
		return FetchResult{}, err
	}

	return FetchResult{
		Entries:      entries,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
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
	Author      string `xml:"author"`
	// Creator matches Dublin Core's <dc:creator>, the de facto author field
	// for feeds (e.g. WordPress) that don't use RSS's own <author>.
	// encoding/xml matches by local name regardless of namespace prefix, so
	// this also covers <creator> without a dc: prefix.
	Creator        string        `xml:"creator"`
	Categories     []string      `xml:"category"`
	Enclosure      *rssEnclosure `xml:"enclosure"`
	MediaThumbnail *rssMediaURL  `xml:"thumbnail"`
	MediaContent   []rssMediaURL `xml:"content"`
}

// rssEnclosure is RSS's standard <enclosure url=".." type="image/..">,
// commonly used to attach a lead image to an item.
type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// rssMediaURL covers the Media RSS namespace's <media:thumbnail> and
// <media:content>, both of which carry a url attribute and (for content)
// optional medium/type attributes identifying it as an image. encoding/xml
// matches by local name regardless of namespace prefix, so this also
// matches Atom's namespaced equivalents if a feed mixes vocabularies.
type rssMediaURL struct {
	URL    string `xml:"url,attr"`
	Medium string `xml:"medium,attr"`
	Type   string `xml:"type,attr"`
}

func (m rssMediaURL) isImage() bool {
	return strings.HasPrefix(m.Medium, "image") || strings.HasPrefix(m.Type, "image/")
}

// entryImage picks the first available image source from an RSS item's
// enclosure or Media RSS thumbnail/content elements.
func entryImage(enclosure *rssEnclosure, thumbnail *rssMediaURL, content []rssMediaURL) string {
	if enclosure != nil && strings.HasPrefix(enclosure.Type, "image/") && enclosure.URL != "" {
		return enclosure.URL
	}
	if thumbnail != nil && thumbnail.URL != "" {
		return thumbnail.URL
	}
	for _, c := range content {
		if c.isImage() && c.URL != "" {
			return c.URL
		}
	}
	return ""
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Links      []atomLink     `xml:"link"`
	Summary    string         `xml:"summary"`
	Content    string         `xml:"content"`
	Updated    string         `xml:"updated"`
	Published  string         `xml:"published"`
	Author     atomAuthor     `xml:"author"`
	Categories []atomCategory `xml:"category"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

// atomCategory is Atom's <category term="..."/>; Term is the human-readable
// label (RSS's <category> has no separate machine label, so RSS and Atom
// categories are normalized to the same []string on Entry).
type atomCategory struct {
	Term string `xml:"term,attr"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

// primaryLink returns the entry's main article link: the "alternate" rel,
// or the first link if none is explicitly marked alternate (Atom defaults
// an unmarked link's rel to "alternate").
func primaryLink(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
	}
	if len(links) > 0 {
		return links[0].Href
	}
	return ""
}

// enclosureImage returns an Atom entry's image enclosure link, if any
// (<link rel="enclosure" type="image/..">).
func enclosureImage(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "enclosure" && strings.HasPrefix(l.Type, "image/") {
			return l.Href
		}
	}
	return ""
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

// trimCategories trims each category and drops any that are blank, so
// whitespace-only <category> elements don't surface as empty entries.
func trimCategories(raw []string) []string {
	categories := make([]string, 0, len(raw))
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c != "" {
			categories = append(categories, c)
		}
	}
	return categories
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

		author := strings.TrimSpace(item.Author)
		if author == "" {
			author = strings.TrimSpace(item.Creator)
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(item.Link),
			Description: strings.TrimSpace(item.Description),
			Image:       strings.TrimSpace(entryImage(item.Enclosure, item.MediaThumbnail, item.MediaContent)),
			Author:      author,
			Categories:  trimCategories(item.Categories),
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

		link := primaryLink(item.Links)

		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(link)
		}
		if id == "" {
			id = strings.TrimSpace(item.Title)
		}

		description := strings.TrimSpace(item.Summary)
		if description == "" {
			description = strings.TrimSpace(item.Content)
		}

		categoryTerms := make([]string, len(item.Categories))
		for i, c := range item.Categories {
			categoryTerms[i] = c.Term
		}

		entries = append(entries, Entry{
			ID:          id,
			Title:       strings.TrimSpace(item.Title),
			Link:        strings.TrimSpace(link),
			Description: description,
			Image:       strings.TrimSpace(enclosureImage(item.Links)),
			Author:      strings.TrimSpace(item.Author.Name),
			Categories:  trimCategories(categoryTerms),
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
