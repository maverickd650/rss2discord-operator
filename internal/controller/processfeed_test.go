/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"k8s.io/client-go/tools/events"
)

// TestProcessFeed_InvalidFilterRegex asserts a feed with an unparsable
// filter regex records the compile error on the feed's status and sends
// nothing, instead of panicking or silently matching everything.
func TestProcessFeed_InvalidFilterRegex(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-regex", "fg-bad-regex"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Filter = &v1alpha1.Filter{Regex: "("}
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, rateLimitRetryAfter := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry for a deterministic regex compile error")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid filter regex")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_InvalidMessageTemplate asserts a feed whose Format fails
// to parse as a template is not retried -- like the filter-regex case above,
// a template compile error is a deterministic FeedSpec misconfiguration that
// won't resolve itself on the normal/retry schedule, so retrying would just
// pin the group at RetryInterval cadence forever -- and is recorded on
// status.
func TestProcessFeed_InvalidMessageTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-template", "fg-bad-template"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Unclosed")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry for a deterministic message template compile error")
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_InvalidForumThreadNameTemplate asserts a feed with a
// valid message Format but an unparsable ForumThreadName template is not
// retried and the error is recorded, exercising the thread-name compile
// branch distinct from the message-template branch above.
func TestProcessFeed_InvalidForumThreadNameTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-bad-thread-name", "fg-bad-thread-name"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.ForumThreadName = "{{.Unclosed"
	fg.Spec.Feeds[0] = feed
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry for a deterministic forum thread name template compile error")
	}
	if feedStatusFor(fg, feed.RSSUrl).LastError == "" {
		t.Fatal("expected LastError to be set for the invalid forum thread name template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_NotModifiedPersistsLastModified asserts a 304 response
// carrying a Last-Modified validator (rather than an ETag) is still stored,
// since only the ETag persist path had previously been exercised.
func TestProcessFeed_NotModifiedPersistsLastModified(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-not-modified-lm", "fg-not-modified-lm"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := rss.FetchResult{NotModified: true, LastModified: "Fri, 23 Oct 2015 07:28:00 GMT"}

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, nil, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry on a 304 response")
	}
	if got := feedStatusFor(fg, feed.RSSUrl).LastModified; got != fetchResult.LastModified {
		t.Fatalf("LastModified[%q] = %q, want %q", feed.RSSUrl, got, fetchResult.LastModified)
	}
}

// TestProcessFeed_LastCheckedTracksSuccessNotAttempts asserts LastChecked
// advances on a successful check -- including a 304 -- but not on a failed
// fetch, so it reads as "last time we confirmed this feed's state" rather
// than "last time we attempted to reach it".
func TestProcessFeed_LastCheckedTracksSuccessNotAttempts(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-lastchecked", "fg-lastchecked"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, errors.New("boom"), nil, "2026-01-01T00:00:00Z")
	if got := feedStatusFor(fg, feed.RSSUrl).LastChecked; got != "" {
		t.Fatal("expected LastChecked not to be set after a fetch error")
	}

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{NotModified: true}, nil, nil, "2026-01-01T00:00:00Z")
	if got := feedStatusFor(fg, feed.RSSUrl).LastChecked; got != "2026-01-01T00:00:00Z" {
		t.Fatalf("LastChecked after a 304 = %q, want it set to the check time", got)
	}
}

// TestProcessFeed_AlreadySentEntrySkipped asserts an entry already recorded
// in LastSent is not re-delivered, exercising the dedup short-circuit
// independently of any filter/template path.
func TestProcessFeed_AlreadySentEntrySkipped(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-dedup", "fg-dedup"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	feedStatusFor(fg, feed.RSSUrl).LastSent = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, client, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry for an already-sent entry")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected the already-sent entry to be skipped, got %d messages", discordServer.MessageCount())
	}
}

// TestProcessFeed_RecoversFromStaleErrorWithNoNewEntries asserts a feed that
// previously failed (LastError/RetryCount/BackoffUntil set) but now fetches
// successfully with only already-sent/filtered entries -- so nothing is
// pending -- has its error state cleared. Without this, a feed that recovers
// but happens to have nothing new to deliver would show a stale error (and
// keep the FeedGroup's Ready condition False) indefinitely, since neither
// the 304 nor the empty-entries branch runs for a fetch that did return
// entries, all of which were already handled.
func TestProcessFeed_RecoversFromStaleErrorWithNoNewEntries(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-recovers", "fg-recovers"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	fs := feedStatusFor(fg, feed.RSSUrl)
	fs.LastSent = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
	fs.LastError = "previous attempt: connection refused"
	fs.RetryCount = 3
	fs.BackoffUntil = "2025-12-31T01:00:00Z"
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, client, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry once the feed recovers")
	}
	got := feedStatusFor(fg, feed.RSSUrl)
	if got.LastError != "" {
		t.Fatalf("expected LastError to clear, got %q", got.LastError)
	}
	if got.RetryCount != 0 {
		t.Fatalf("expected RetryCount to reset, got %d", got.RetryCount)
	}
	if got.BackoffUntil != "" {
		t.Fatalf("expected BackoffUntil to clear, got %q", got.BackoffUntil)
	}
}

// TestPermanentBackoffDuration verifies the exponential formula and cap.
func TestPermanentBackoffDuration(t *testing.T) {
	base := 5 * time.Minute
	cases := []struct {
		retryCount int
		wantCapped bool
	}{
		{retryCount: 1, wantCapped: false}, // 5m * 2^1 = 10m
		{retryCount: 2, wantCapped: false}, // 5m * 2^2 = 20m
		{retryCount: 6, wantCapped: false}, // 5m * 2^6 = 320m < 6h
		{retryCount: 7, wantCapped: true},  // 5m * 2^7 = 640m > 6h
		{retryCount: 62, wantCapped: true}, // overflow guard
		{retryCount: 63, wantCapped: true}, // overflow guard
	}
	for _, tc := range cases {
		got := permanentBackoffDuration(tc.retryCount, base)
		if tc.wantCapped {
			if got != maxPermanentBackoff {
				t.Errorf("retryCount=%d: got %v, want cap %v", tc.retryCount, got, maxPermanentBackoff)
			}
		} else {
			if got >= maxPermanentBackoff {
				t.Errorf("retryCount=%d: got %v >= cap %v, expected uncapped", tc.retryCount, got, maxPermanentBackoff)
			}
			want := base * (1 << uint(tc.retryCount))
			if got != want {
				t.Errorf("retryCount=%d: got %v, want %v", tc.retryCount, got, want)
			}
		}
	}
}

// TestPermanentBackoffDuration_ClampsRetryCountBelowOne asserts a retryCount
// of zero (or negative) is treated the same as 1, so a caller can never get a
// larger backoff than the first retry by passing an unclamped count.
func TestPermanentBackoffDuration_ClampsRetryCountBelowOne(t *testing.T) {
	base := 5 * time.Minute
	want := permanentBackoffDuration(1, base)

	if got := permanentBackoffDuration(0, base); got != want {
		t.Errorf("retryCount=0: got %v, want %v (same as retryCount=1)", got, want)
	}
	if got := permanentBackoffDuration(-1, base); got != want {
		t.Errorf("retryCount=-1: got %v, want %v (same as retryCount=1)", got, want)
	}
}

// TestProcessFeed_PermanentFetchFailureSetsBackoff asserts a permanent fetch
// error (HTTP 404) sets BackoffUntil to an exponential offset, returns no
// wantRetry (the group's normal interval is unaffected), and does not set a
// sentinel on the first few retries.
func TestProcessFeed_PermanentFetchFailureSetsBackoff(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-backoff", "fg-perm-backoff"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	before := time.Now().UTC()
	wantRetry, rateLimitRetryAfter := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, notFoundErr, nil, "2026-01-01T00:00:00Z")
	after := time.Now().UTC()

	if wantRetry {
		t.Fatal("expected no group-level retry for a permanent fetch failure")
	}
	if rateLimitRetryAfter != 0 {
		t.Fatalf("expected no rate-limit backoff, got %v", rateLimitRetryAfter)
	}

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil == "" {
		t.Fatal("expected BackoffUntil to be set after a permanent failure")
	}
	if fs.BackoffUntil == permanentBackoffSentinel {
		t.Fatal("expected a concrete timestamp, not sentinel, on first failure")
	}
	until, err := time.Parse(time.RFC3339, fs.BackoffUntil)
	if err != nil {
		t.Fatalf("BackoffUntil %q is not RFC3339: %v", fs.BackoffUntil, err)
	}
	// RetryCount is 1 after first failure; backoff = 5m * 2^1 = 10m.
	// RFC3339 truncates to seconds, so expand the window by one second on
	// each side to avoid spurious failures when the wall clock sits near a
	// second boundary.
	wantMin := before.Add(10 * time.Minute).Truncate(time.Second)
	wantMax := after.Add(10 * time.Minute).Add(time.Second)
	if until.Before(wantMin) || until.After(wantMax) {
		t.Errorf("BackoffUntil %v outside expected window [%v, %v]", until, wantMin, wantMax)
	}
}

// TestProcessFeed_PermanentFetchFailureCapSetsSentinel asserts that once the
// exponential backoff exceeds 6h, BackoffUntil is set to the sentinel and the
// Warning Event fires.
func TestProcessFeed_PermanentFetchFailureCapSetsSentinel(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-sentinel", "fg-perm-sentinel"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fg.Spec.RetryInterval = "5m"
	// RetryCount=6 means after the increment it becomes 7, and
	// 5m * 2^7 = 640m > 6h, which should trigger the sentinel.
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 6
	notFoundErr := &rss.HTTPStatusError{StatusCode: 404, Status: http404Status}

	recorder := events.NewFakeRecorder(10)
	wantRetry, _ := (&FeedGroupReconciler{Recorder: recorder}).processFeed(
		ctx, fg, feed, rss.FetchResult{}, notFoundErr, nil, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no group-level retry for a capped permanent failure")
	}
	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil != permanentBackoffSentinel {
		t.Errorf("BackoffUntil = %q, want sentinel %q", fs.BackoffUntil, permanentBackoffSentinel)
	}
	select {
	case <-recorder.Events:
		// good: Warning Event fired
	default:
		t.Fatal("expected Warning Event when sentinel is set")
	}
}

// TestProcessFeed_PermanentFailureClearedOnSuccess asserts that a successful
// fetch (including a 304) clears BackoffUntil set by a previous permanent
// failure.
func TestProcessFeed_PermanentFailureClearedOnSuccess(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-perm-clear", "fg-perm-clear"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	// Simulate a feed that was previously in permanent backoff.
	feedStatusFor(fg, feed.RSSUrl).BackoffUntil = "2030-01-01T00:00:00Z"
	feedStatusFor(fg, feed.RSSUrl).RetryCount = 3

	(&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, rss.FetchResult{NotModified: true}, nil, nil, "2026-01-01T00:00:00Z")

	fs := feedStatusFor(fg, feed.RSSUrl)
	if fs.BackoffUntil != "" {
		t.Errorf("BackoffUntil = %q, want empty after successful check", fs.BackoffUntil)
	}
	if fs.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0 after successful check", fs.RetryCount)
	}
}

// TestClearPermanentBackoffs asserts the helper resets BackoffUntil and
// RetryCount on every feed, including those not in backoff, so the first
// permanentBackoffDuration call after a spec change uses a fresh RetryCount.
func TestClearPermanentBackoffs(t *testing.T) {
	fg, _ := newMetricsFeedGroup("clear-backoff-ns", "clear-backoff", "")
	fg.Spec.Feeds = append(fg.Spec.Feeds, v1alpha1.FeedSpec{RSSUrl: "https://other.example.com/feed.xml"})
	ensureFeedStatuses(fg)

	feedStatusFor(fg, exampleFeedURL).BackoffUntil = permanentBackoffSentinel
	feedStatusFor(fg, exampleFeedURL).RetryCount = 7
	feedStatusFor(fg, "https://other.example.com/feed.xml").RetryCount = 2

	clearPermanentBackoffs(fg)

	for _, fs := range fg.Status.Feeds {
		if fs.BackoffUntil != "" {
			t.Errorf("feed %s: BackoffUntil = %q, want empty", fs.RSSUrl, fs.BackoffUntil)
		}
		if fs.RetryCount != 0 {
			t.Errorf("feed %s: RetryCount = %d, want 0", fs.RSSUrl, fs.RetryCount)
		}
	}
}
