package controller

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// shortContent is reused across truncation test cases below.
const shortContent = "short"

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{name: "hash prefix", input: "#00FF00", want: 0x00FF00},
		{name: "no prefix", input: "FF0000", want: 0xFF0000},
		{name: "lowercase", input: "#ffffff", want: 0xFFFFFF},
		{name: "blank", input: "  ", want: 0},
		{name: "malformed", input: "#nothex", want: 0},
		{name: "with surrounding space", input: "  #123456  ", want: 0x123456},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHexColor(tc.input); got != tc.want {
				t.Fatalf("parseHexColor(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestHTTPURLOrEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "https kept", input: "https://example.com/a", want: "https://example.com/a"},
		{name: "http kept", input: "http://example.com/a", want: "http://example.com/a"},
		{name: "javascript dropped", input: "javascript:alert(1)", want: ""},
		{name: "data dropped", input: "data:text/html,<script>", want: ""},
		{name: "empty dropped", input: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpURLOrEmpty(tc.input); got != tc.want {
				t.Fatalf("httpURLOrEmpty(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain text unchanged", input: "plain text body", want: "plain text body"},
		{name: "paragraphs become blank lines", input: "<p>one</p><p>two</p>", want: "one\n\ntwo"},
		{name: "br becomes newline", input: "a<br>b", want: "a\nb"},
		{name: "inline tags stripped", input: "<b>bold</b> text", want: "bold text"},
		{name: "entities unescaped", input: "a &amp; b &lt;c&gt;", want: "a & b <c>"},
		{name: "collapses excess blank lines", input: "<p>a</p><br><br><br><p>b</p>", want: "a\n\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Fatalf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	cases := []struct {
		name    string
		content string
		max     int
		want    string
	}{
		{name: "under limit unchanged", content: shortContent, max: 10, want: shortContent},
		{name: "exactly at limit unchanged", content: shortContent, max: 5, want: shortContent},
		{name: "truncated with ellipsis", content: "truncate me", max: 5, want: "trun…"},
		{name: "multibyte trimmed by rune", content: "日本語テスト", max: 3, want: "日本…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateMessage(tc.content, tc.max); got != tc.want {
				t.Fatalf("truncateMessage(%q, %d) = %q, want %q", tc.content, tc.max, got, tc.want)
			}
		})
	}
}

func TestParseDurationWithDefault(t *testing.T) {
	fallback := 5 * time.Minute

	t.Run("blank uses fallback", func(t *testing.T) {
		got, err := parseDurationWithDefault("  ", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != fallback {
			t.Fatalf("got %v, want fallback %v", got, fallback)
		}
	})

	t.Run("valid duration parsed", func(t *testing.T) {
		got, err := parseDurationWithDefault("90s", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 90*time.Second {
			t.Fatalf("got %v, want 90s", got)
		}
	})

	t.Run("invalid duration returns fallback and error", func(t *testing.T) {
		got, err := parseDurationWithDefault("nope", fallback)
		if err == nil {
			t.Fatal("expected error for invalid duration, got nil")
		}
		if got != fallback {
			t.Fatalf("expected fallback on error, got %v", got)
		}
	})
}

func TestMaxRetryCount(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{in: -1, want: 1},
		{in: 0, want: 1},
		{in: 1, want: 1},
		{in: 5, want: 5},
	}
	for _, tc := range cases {
		if got := maxRetryCount(tc.in); got != tc.want {
			t.Fatalf("maxRetryCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLimitCatchUp(t *testing.T) {
	mk := func(n int) []rss.Entry {
		entries := make([]rss.Entry, n)
		for i := range entries {
			entries[i] = rss.Entry{ID: string(rune('a' + i))}
		}
		return entries
	}

	t.Run("non-positive limit falls back to default", func(t *testing.T) {
		got := limitCatchUp(mk(10), 0)
		if len(got) != defaultCatchUpLimit {
			t.Fatalf("expected %d entries, got %d", defaultCatchUpLimit, len(got))
		}
		// Keeps the most recent (tail) entries.
		if got[len(got)-1].ID != mk(10)[9].ID {
			t.Fatalf("expected most recent entry retained, got %q", got[len(got)-1].ID)
		}
	})

	t.Run("under limit returns all", func(t *testing.T) {
		got := limitCatchUp(mk(3), 5)
		if len(got) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(got))
		}
	})

	t.Run("trims to limit keeping newest", func(t *testing.T) {
		got := limitCatchUp(mk(10), 2)
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		if got[0].ID != "i" || got[1].ID != "j" {
			t.Fatalf("expected last two entries, got %q,%q", got[0].ID, got[1].ID)
		}
	})
}

func TestPruneLastSent(t *testing.T) {
	t.Run("under cap unchanged", func(t *testing.T) {
		sent := map[string]string{"a": "2024-01-01T00:00:00Z", "b": "2024-01-02T00:00:00Z"}
		pruneLastSent(sent, 5)
		if len(sent) != 2 {
			t.Fatalf("expected map unchanged, got %d entries", len(sent))
		}
	})

	t.Run("drops oldest by timestamp", func(t *testing.T) {
		sent := map[string]string{
			"oldest": "2024-01-01T00:00:00Z",
			"middle": "2024-01-02T00:00:00Z",
			"newest": "2024-01-03T00:00:00Z",
		}
		pruneLastSent(sent, 2)
		if len(sent) != 2 {
			t.Fatalf("expected 2 entries after prune, got %d", len(sent))
		}
		if _, ok := sent["oldest"]; ok {
			t.Fatal("expected the oldest entry to be pruned")
		}
		if _, ok := sent["newest"]; !ok {
			t.Fatal("expected the newest entry to be retained")
		}
	})
}

func TestComputeEntryKey(t *testing.T) {
	const entryLink = "http://x"
	a := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	b := computeEntryKey(rss.Entry{ID: "1", Link: entryLink, Title: "T"})
	c := computeEntryKey(rss.Entry{ID: "2", Link: entryLink, Title: "T"})

	if a != b {
		t.Fatal("expected identical entries to produce the same key")
	}
	if a == c {
		t.Fatal("expected differing entries to produce different keys")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char hex sha256, got %d chars", len(a))
	}
}

func TestCompileFilterRegex(t *testing.T) {
	t.Run("nil filter yields nil regex", func(t *testing.T) {
		re, err := compileFilterRegex(nil)
		if err != nil || re != nil {
			t.Fatalf("expected nil,nil got %v,%v", re, err)
		}
	})

	t.Run("blank regex yields nil regex", func(t *testing.T) {
		re, err := compileFilterRegex(&v1alpha1.Filter{Regex: "  "})
		if err != nil || re != nil {
			t.Fatalf("expected nil,nil got %v,%v", re, err)
		}
	})

	t.Run("valid regex compiled", func(t *testing.T) {
		re, err := compileFilterRegex(&v1alpha1.Filter{Regex: "foo.*"})
		if err != nil || re == nil {
			t.Fatalf("expected compiled regex, got %v,%v", re, err)
		}
	})

	t.Run("invalid regex returns error", func(t *testing.T) {
		_, err := compileFilterRegex(&v1alpha1.Filter{Regex: "("})
		if err == nil {
			t.Fatal("expected error for invalid regex, got nil")
		}
	})
}

func TestMatchesFilter(t *testing.T) {
	entry := rss.Entry{Title: "Breaking News", Description: "something happened"}

	t.Run("nil filter matches", func(t *testing.T) {
		if !matchesFilter(nil, entry, nil) {
			t.Fatal("expected nil filter to match everything")
		}
	})

	t.Run("regex non-match rejects", func(t *testing.T) {
		re, _ := compileFilterRegex(&v1alpha1.Filter{Regex: "nomatch"})
		if matchesFilter(&v1alpha1.Filter{Regex: "nomatch"}, entry, re) {
			t.Fatal("expected non-matching regex to reject entry")
		}
	})

	t.Run("keyword case-insensitive match", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"BREAKING"}}
		if !matchesFilter(f, entry, nil) {
			t.Fatal("expected case-insensitive keyword match")
		}
	})

	t.Run("no keyword match rejects", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"weather"}}
		if matchesFilter(f, entry, nil) {
			t.Fatal("expected no keyword match to reject")
		}
	})

	t.Run("blank keywords skipped, empty keyword list matches", func(t *testing.T) {
		f := &v1alpha1.Filter{Keywords: []string{"  "}}
		// All keywords are blank/skipped, so none match.
		if matchesFilter(f, entry, nil) {
			t.Fatal("expected all-blank keywords to match nothing")
		}
	})
}

func TestResolveEmbedSpec(t *testing.T) {
	feedEmbed := &v1alpha1.EmbedSpec{Color: "#feed00"}
	groupEmbed := &v1alpha1.EmbedSpec{Color: "#9009ff"}

	t.Run("feed embed takes precedence", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{Embed: feedEmbed})
		if got != feedEmbed {
			t.Fatal("expected feed embed to take precedence")
		}
	})

	t.Run("falls back to group embed", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{})
		if got != groupEmbed {
			t.Fatal("expected fallback to group embed")
		}
	})

	t.Run("nil when neither set", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

func TestCompileMessageTemplate_Precedence(t *testing.T) {
	t.Run("feed format wins", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Format: "group {{.Title}}"}}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "feed {{.Title}}"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var sb strings.Builder
		if err := tmpl.Execute(&sb, map[string]string{"Title": "X"}); err != nil {
			t.Fatalf("execute error: %v", err)
		}
		if sb.String() != "feed X" {
			t.Fatalf("expected feed format used, got %q", sb.String())
		}
	})

	t.Run("falls back to default when both blank", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tmpl == nil {
			t.Fatal("expected a compiled default template")
		}
	})

	t.Run("invalid template returns error", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if _, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "{{.Unclosed"}); err == nil {
			t.Fatal("expected parse error for invalid template, got nil")
		}
	})
}
