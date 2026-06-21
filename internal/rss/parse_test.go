package rss

import (
	"testing"
	"time"
)

// Shared literals reused across table-driven cases below; pulled into
// constants so repeated values don't trip goconst.
const (
	relAlternate = "alternate"
	relEnclosure = "enclosure"
	imageJPEG    = "image/jpeg"
	picJPEGURL   = "http://pic.jpg"
	thumbJPEGURL = "http://thumb.jpg"
	encURL       = "http://enc"
)

func TestParseTime(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
		// wantZero is true when a successful parse should yield the zero time
		// (i.e. blank input), distinct from an error.
		wantZero bool
	}{
		{name: "blank", value: "   ", wantZero: true},
		{name: "RFC3339", value: "2015-10-21T07:28:00Z"},
		{name: "RFC1123Z", value: "Wed, 21 Oct 2015 07:28:00 +0000"},
		{name: "RFC1123", value: "Wed, 21 Oct 2015 07:28:00 UTC"},
		{name: "single-digit day", value: "Wed, 2 Jan 2006 15:04:05 MST"},
		{name: "date only", value: "2015-10-21"},
		{name: "datetime no zone", value: "2015-10-21 07:28:00"},
		{name: "unsupported", value: "not a date at all", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTime(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.value, err)
			}
			if tc.wantZero && !got.IsZero() {
				t.Fatalf("expected zero time for %q, got %v", tc.value, got)
			}
			if !tc.wantZero {
				if got.IsZero() {
					t.Fatalf("expected non-zero time for %q", tc.value)
				}
				if got.Location() != time.UTC {
					t.Fatalf("expected parsed time normalized to UTC, got %v", got.Location())
				}
			}
		})
	}
}

func TestPrimaryLink(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: "no links", links: nil, want: ""},
		{
			name:  "explicit alternate preferred over enclosure",
			links: []atomLink{{Href: encURL, Rel: relEnclosure}, {Href: "http://alt", Rel: relAlternate}},
			want:  "http://alt",
		},
		{
			name:  "unmarked rel treated as alternate",
			links: []atomLink{{Href: "http://first"}},
			want:  "http://first",
		},
		{
			name:  "falls back to first when no alternate",
			links: []atomLink{{Href: encURL, Rel: relEnclosure}, {Href: "http://self", Rel: "self"}},
			want:  encURL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryLink(tc.links); got != tc.want {
				t.Fatalf("primaryLink() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnclosureImage(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: "no links", links: nil, want: ""},
		{
			name:  "image enclosure matched",
			links: []atomLink{{Href: picJPEGURL, Rel: relEnclosure, Type: imageJPEG}},
			want:  picJPEGURL,
		},
		{
			name:  "non-image enclosure ignored",
			links: []atomLink{{Href: "http://file.pdf", Rel: relEnclosure, Type: "application/pdf"}},
			want:  "",
		},
		{
			name:  "alternate link with image type ignored",
			links: []atomLink{{Href: picJPEGURL, Rel: relAlternate, Type: imageJPEG}},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := enclosureImage(tc.links); got != tc.want {
				t.Fatalf("enclosureImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRSSMediaURL_IsImage(t *testing.T) {
	cases := []struct {
		name string
		m    rssMediaURL
		want bool
	}{
		{name: "medium image", m: rssMediaURL{Medium: "image"}, want: true},
		{name: "type image", m: rssMediaURL{Type: "image/png"}, want: true},
		{name: "video medium", m: rssMediaURL{Medium: "video"}, want: false},
		{name: "empty", m: rssMediaURL{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.isImage(); got != tc.want {
				t.Fatalf("isImage() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntryImage(t *testing.T) {
	cases := []struct {
		name      string
		enclosure *rssEnclosure
		thumbnail *rssMediaURL
		content   []rssMediaURL
		want      string
	}{
		{name: "nothing set", want: ""},
		{
			name:      "enclosure preferred",
			enclosure: &rssEnclosure{URL: "http://enc.jpg", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      "http://enc.jpg",
		},
		{
			name:      "non-image enclosure skipped, falls to thumbnail",
			enclosure: &rssEnclosure{URL: "http://file.mp3", Type: "audio/mpeg"},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:    "media content image used when no enclosure/thumbnail",
			content: []rssMediaURL{{URL: "http://c.jpg", Medium: "image"}},
			want:    "http://c.jpg",
		},
		{
			name:    "non-image media content ignored",
			content: []rssMediaURL{{URL: "http://c.mp4", Medium: "video"}},
			want:    "",
		},
		{
			name:      "enclosure with empty url skipped",
			enclosure: &rssEnclosure{URL: "", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryImage(tc.enclosure, tc.thumbnail, tc.content); got != tc.want {
				t.Fatalf("entryImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseFeed_AtomEntry(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>urn:uuid:1</id>
<title>Atom Title</title>
<link rel="alternate" href="http://example.com/a" />
<updated>2015-10-21T07:28:00Z</updated>
<content>Body content</content>
</entry>
</feed>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.ID != "urn:uuid:1" || e.Title != "Atom Title" || e.Link != "http://example.com/a" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	// Summary is empty, so Content is used as the description.
	if e.Description != "Body content" {
		t.Fatalf("expected description from content, got %q", e.Description)
	}
	// No published element, so the updated timestamp is used.
	if e.Published.IsZero() {
		t.Fatal("expected published derived from updated timestamp")
	}
}

func TestParseFeed_AtomResolvesRelativeLinksAgainstXMLBase(t *testing.T) {
	// Atom feeds commonly give a relative href for <link> (and an enclosure
	// image) alongside an xml:base, rather than a full URL. Without
	// resolving against it, the relative string is unusable as a link and
	// isn't recognized as a URL by anything downstream that expects an
	// absolute one.
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://example.com/blog/">
<entry>
<id>1</id>
<title>Feed-level base</title>
<link rel="alternate" href="2026/article-1"/>
<link rel="enclosure" type="image/jpeg" href="img/1.jpg"/>
</entry>
<entry xml:base="https://other.example.com/news/">
<id>2</id>
<title>Entry-level base overrides feed-level</title>
<link rel="alternate" href="article-2"/>
</entry>
<entry>
<id>3</id>
<title>Already absolute</title>
<link rel="alternate" href="https://elsewhere.example.com/x"/>
</entry>
</feed>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if want := "https://example.com/blog/2026/article-1"; entries[0].Link != want {
		t.Fatalf("expected link %q, got %q", want, entries[0].Link)
	}
	if want := "https://example.com/blog/img/1.jpg"; entries[0].Image != want {
		t.Fatalf("expected image %q, got %q", want, entries[0].Image)
	}
	if want := "https://other.example.com/news/article-2"; entries[1].Link != want {
		t.Fatalf("expected entry-level base to override feed-level, got %q", entries[1].Link)
	}
	if want := "https://elsewhere.example.com/x"; entries[2].Link != want {
		t.Fatalf("expected already-absolute link unchanged, got %q", entries[2].Link)
	}
}

func TestParseFeed_AtomLeavesRelativeLinkUnchangedWithoutBase(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry><id>1</id><title>No base</title><link rel="alternate" href="/2026/article"/></entry>
</feed>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/2026/article"; entries[0].Link != want {
		t.Fatalf("expected relative link unchanged without xml:base, got %q", entries[0].Link)
	}
}

func TestResolveAtomURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		ref  string
		want string
	}{
		{name: "empty base returns ref unchanged", base: "", ref: "/a", want: "/a"},
		{name: "empty ref returns ref unchanged", base: "http://example.com/", ref: "", want: ""},
		{name: "already absolute ref passes through", base: "http://example.com/", ref: "http://other.com/a", want: "http://other.com/a"},
		{name: "relative ref resolves against base", base: "http://example.com/feed/", ref: "a", want: "http://example.com/feed/a"},
		{name: "malformed base returns ref unchanged", base: "%zz", ref: "/a", want: "/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAtomURL(tc.base, tc.ref); got != tc.want {
				t.Fatalf("resolveAtomURL(%q, %q) = %q, want %q", tc.base, tc.ref, got, tc.want)
			}
		})
	}
}

func TestParseFeed_RSSFallsBackToLinkAndTitleForID(t *testing.T) {
	// No guid, so the link is used as the ID.
	data := []byte(`<?xml version="1.0"?>
<rss><channel>
<item><title>NoGuid</title><link>http://example.com/link</link></item>
<item><title>OnlyTitle</title></item>
</channel></rss>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "http://example.com/link" {
		t.Fatalf("expected link used as ID, got %q", entries[0].ID)
	}
	if entries[1].ID != "OnlyTitle" {
		t.Fatalf("expected title used as ID, got %q", entries[1].ID)
	}
}

func TestParseFeed_InvalidXML(t *testing.T) {
	if _, err := parseFeed([]byte("<not-closed")); err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

func TestParseFeed_RSSAuthorAndCategories(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<rss><channel>
<item>
  <title>WithAuthor</title>
  <author>jane@example.com (Jane Doe)</author>
  <category>Go</category>
  <category>  </category>
  <category>Kubernetes</category>
</item>
<item>
  <title>WithCreator</title>
  <creator>Dublin Core Jane</creator>
</item>
</channel></rss>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Author != "jane@example.com (Jane Doe)" {
		t.Fatalf("unexpected author: %q", entries[0].Author)
	}
	if want := []string{"Go", "Kubernetes"}; !slicesEqual(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
	}

	// No <author>, so <creator> (Dublin Core's de facto author field) is used.
	if entries[1].Author != "Dublin Core Jane" {
		t.Fatalf("expected creator fallback for author, got %q", entries[1].Author)
	}
}

func TestParseFeed_AtomAuthorAndCategories(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>1</id>
<title>Atom With Author</title>
<author><name>John Smith</name></author>
<category term="golang" />
<category term="" />
<category term="kubernetes" />
</entry>
</feed>`)

	entries, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Author != "John Smith" {
		t.Fatalf("unexpected author: %q", entries[0].Author)
	}
	if want := []string{"golang", "kubernetes"}; !slicesEqual(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
	}
}

func TestTrimCategories(t *testing.T) {
	got := trimCategories([]string{"  a  ", "", "b", "   "})
	if want := []string{"a", "b"}; !slicesEqual(got, want) {
		t.Fatalf("trimCategories() = %v, want %v", got, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseFeed_UnknownRootFallsBackToAtom(t *testing.T) {
	// Root element is neither <rss> nor <feed>; parseRSS yields no items
	// without error, so an Atom-shaped document under an odd root still
	// parses via the fallback path.
	data := []byte(`<?xml version="1.0"?>
<feedwrapper><entry><id>1</id><title>X</title></entry></feedwrapper>`)
	if _, err := parseFeed(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseFeed_DecodesNonUTF8Encodings(t *testing.T) {
	// encoding/xml has no charset support of its own and errors out on any
	// non-UTF-8 declared encoding unless a CharsetReader is wired up; older
	// or non-English-language publishers commonly declare ISO-8859-1 or
	// windows-1252, so a feed in either must still parse and decode its
	// Latin-1 bytes (here 0xE9, "é") to the correct UTF-8 rune.
	cases := []struct {
		name     string
		encoding string
	}{
		{name: "ISO-8859-1", encoding: "ISO-8859-1"},
		{name: "windows-1252", encoding: "windows-1252"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("<?xml version=\"1.0\" encoding=\"" + tc.encoding + "\"?>" +
				"<rss><channel><item><title>Caf\xe9</title><guid>1</guid></item></channel></rss>")

			entries, err := parseFeed(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			if want := "Café"; entries[0].Title != want {
				t.Fatalf("expected title %q, got %q", want, entries[0].Title)
			}
		})
	}
}
