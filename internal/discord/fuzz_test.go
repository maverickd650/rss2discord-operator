package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzEmbedPayload exercises embedToPayload/clampEmbedTotalLength/
// truncateRunes -- and the json.Marshal of the resulting payload -- against
// arbitrary embed field strings. Feed entry titles/descriptions/authors are
// untrusted external text (the same threat model as FuzzParseFeed in
// internal/rss), so these fields must never panic or defeat Discord's
// length/mention-suppression guarantees regardless of how adversarial the
// input is. Go fuzz-generated strings, unlike string literals, aren't
// required to be valid UTF-8, so seeds/invariants below account for that.
func FuzzEmbedPayload(f *testing.F) {
	seeds := []string{
		"",
		"plain title",
		"@everyone",
		"@here",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"\x00\x00\x00",
		"\xff\xfe\x80", // invalid UTF-8 byte sequence
		strings.Repeat("x", maxEmbedTotalLength*2),
		strings.Repeat("\U0001F600", maxEmbedTotalLength), // 4-byte emoji straddling the clamp boundary
		"é́́",    // base char with combining acute accents
		"日本語テスト", // multibyte (Japanese) text
	}
	for _, title := range seeds {
		for _, description := range seeds {
			f.Add(title, description, "author", "footer", "https://example.com")
		}
	}

	f.Fuzz(func(t *testing.T, title, description, author, footer, url string) {
		e := Embed{Title: title, Description: description, AuthorName: author, FooterText: footer, URL: url}

		clamped := clampEmbedTotalLength(e)
		if overflow := EmbedTotalLengthOverflow(clamped); overflow != 0 {
			// clampEmbedTotalLength only ever trims Description (see its doc
			// comment: title/footer/author are "short in practice" for real
			// callers -- feedgroup_controller.go pre-truncates title to 256
			// runes, and footer/author are CRD MaxLength-capped). So the one
			// case where overflow can legitimately remain is title+footer+
			// author alone already exceeding the cap with nothing left in
			// Description to cut -- anything else is a real clamp bug.
			nonDescRunes := len([]rune(e.Title)) + len([]rune(e.FooterText)) + len([]rune(e.AuthorName))
			if clamped.Description != "" {
				t.Fatalf("clampEmbedTotalLength left an overflow of %d without trimming Description to empty: %+v", overflow, clamped)
			}
			if nonDescRunes <= maxEmbedTotalLength {
				t.Fatalf("clampEmbedTotalLength left an overflow of %d even though title+footer+author alone (%d runes) fit within the limit: %+v", overflow, nonDescRunes, e)
			}
		}

		// truncateRunes itself is unconditionally UTF-8-safe: it operates on
		// an already-decoded []rune (which replaces any invalid input bytes
		// with U+FFFD during the string->[]rune conversion), so re-encoding
		// via string(runes) is always valid regardless of how filthy the
		// original bytes were. clampEmbedTotalLength's pass-through branch
		// (no overflow, nothing trimmed) does *not* go through that
		// round-trip, so it carries no such guarantee -- this checks the
		// helper directly rather than asserting UTF-8 validity on
		// clampEmbedTotalLength's untouched pass-through output.
		descRunes := []rune(description)
		truncated := truncateRunes(descRunes, maxEmbedTotalLength)
		if got := len([]rune(truncated)); got > maxEmbedTotalLength {
			t.Fatalf("truncateRunes(%d runes, %d) returned %d runes", len(descRunes), maxEmbedTotalLength, got)
		}
		if !utf8.ValidString(truncated) {
			t.Fatalf("truncateRunes output is not valid UTF-8: %q", truncated)
		}

		payload := embedToPayload(e)
		full := discordPayload{
			Embeds:          []discordEmbed{payload},
			AllowedMentions: discordAllowedMentions{Parse: []string{}},
		}
		body, err := json.Marshal(full)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		// The @everyone/@here suppression invariant: allowed_mentions must
		// never be omitted or nil, regardless of what ends up in the embed.
		if !strings.Contains(string(body), `"allowed_mentions":{"parse":[]}`) {
			t.Fatalf("marshaled payload missing empty allowed_mentions.parse: %s", body)
		}
		// json.Marshal replaces invalid UTF-8 with U+FFFD rather than
		// erroring, so the wire payload is always well-formed JSON
		// regardless of input filth -- confirm it actually round-trips.
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("marshaled payload does not round-trip through json.Unmarshal: %v\n%s", err, body)
		}
	})
}
