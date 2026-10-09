package rss

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// referenceParseTime is the original parseTime: nine layouts tried strictly
// in order. parseTime now picks a layout group from the value's shape to
// avoid a failed time.Parse (and its error allocation) per date, and must
// accept exactly the same inputs and return the same instants.
func referenceParseTime(value string) (time.Time, error) {
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

var parseTimeCorpus = []string{
	"",
	" \t ",
	"2016-11-22T08:29:01Z",
	"2015-10-21T07:28:00+02:00",
	"2015-10-21T07:28:00.123456Z",
	"2015-10-21 07:28:00",
	"2016-11-22",
	"Tue, 22 Nov 2016 08:29:01 +0000",
	"Wed, 21 Oct 2015 07:28:00 -0700",
	"Wed, 21 Oct 2015 07:28:00 UTC",
	"Wed, 21 Oct 2015 07:28:00 GMT",
	"Wed, 21 Oct 2015 07:28:00 EST",
	"Wed, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 06 15:04 MST",
	"02 Jan 06 15:04 MST",
	"02 Jan 06 15:04 -0700",
	"Mon Jan  2 15:04:05 2006",
	"Mon Jan 2 15:04:05 2006",
	"not a date at all",
	"2015-13-45",
	"2015-10-21T",
	"21 Oct 2015",
	"Wed",
	"20151021",
	"2015/10/21",
	"\t2015-10-21T07:28:00Z\n",
}

func TestParseTimeMatchesReference(t *testing.T) {
	for _, in := range parseTimeCorpus {
		got, gotErr := parseTime(in)
		want, wantErr := referenceParseTime(in)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("parseTime(%q) err = %v, reference err = %v", in, gotErr, wantErr)
		}
		if !got.Equal(want) {
			t.Fatalf("parseTime(%q) = %v, reference = %v", in, got, want)
		}
	}
}

func FuzzParseTime(f *testing.F) {
	for _, in := range parseTimeCorpus {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, gotErr := parseTime(in)
		want, wantErr := referenceParseTime(in)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("parseTime(%q) err = %v, reference err = %v", in, gotErr, wantErr)
		}
		if !got.Equal(want) {
			t.Fatalf("parseTime(%q) = %v, reference = %v", in, got, want)
		}
	})
}

func BenchmarkParseTime(b *testing.B) {
	for _, tc := range []struct{ name, in string }{
		{"RFC1123Z", "Tue, 22 Nov 2016 08:29:01 +0000"},
		{"RFC1123", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"RFC3339", "2016-11-22T08:29:01Z"},
		{"dateOnly", "2016-11-22"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = parseTime(tc.in)
			}
		})
	}
}
