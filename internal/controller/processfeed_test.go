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
	"context"
	"testing"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// TestProcessFeed_InvalidFilterRegex asserts a feed with an unparsable
// filter regex records the compile error on the feed's status and sends
// nothing, instead of panicking or silently matching everything.
func TestProcessFeed_InvalidFilterRegex(t *testing.T) {
	ctx := context.Background()
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
	if fg.Status.LastError[feed.RSSUrl] == "" {
		t.Fatal("expected LastError to be set for the invalid filter regex")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_InvalidMessageTemplate asserts a feed whose Format fails
// to parse as a template is retried (the error may be transient-looking to
// the reconciler at this layer; persistent-failure handling lives one level
// up the retry counter) and recorded on status.
func TestProcessFeed_InvalidMessageTemplate(t *testing.T) {
	ctx := context.Background()
	ns, name := "processfeed-bad-template", "fg-bad-template"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "{{.Unclosed")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if !wantRetry {
		t.Fatal("expected retry for an invalid message template")
	}
	if fg.Status.LastError[feed.RSSUrl] == "" {
		t.Fatal("expected LastError to be set for the invalid template")
	}
	if discordServer.MessageCount() != 0 {
		t.Fatalf("expected no message sent, got %d", discordServer.MessageCount())
	}
}

// TestProcessFeed_InvalidForumThreadNameTemplate asserts a feed with a
// valid message Format but an unparsable ForumThreadName template is
// retried and the error is recorded, exercising the thread-name compile
// branch distinct from the message-template branch above.
func TestProcessFeed_InvalidForumThreadNameTemplate(t *testing.T) {
	ctx := context.Background()
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

	if !wantRetry {
		t.Fatal("expected retry for an invalid forum thread name template")
	}
	if fg.Status.LastError[feed.RSSUrl] == "" {
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
	ctx := context.Background()
	ns, name := "processfeed-not-modified-lm", "fg-not-modified-lm"
	defer deleteFeedGroupMetrics(ns, name)

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := rss.FetchResult{NotModified: true, LastModified: "Fri, 23 Oct 2015 07:28:00 GMT"}

	wantRetry, _ := (&FeedGroupReconciler{}).processFeed(
		ctx, fg, feed, fetchResult, nil, nil, "2026-01-01T00:00:00Z")

	if wantRetry {
		t.Fatal("expected no retry on a 304 response")
	}
	if got := fg.Status.FeedLastModified[feed.RSSUrl]; got != fetchResult.LastModified {
		t.Fatalf("FeedLastModified[%q] = %q, want %q", feed.RSSUrl, got, fetchResult.LastModified)
	}
}

// TestProcessFeed_AlreadySentEntrySkipped asserts an entry already recorded
// in LastSent is not re-delivered, exercising the dedup short-circuit
// independently of any filter/template path.
func TestProcessFeed_AlreadySentEntrySkipped(t *testing.T) {
	ctx := context.Background()
	ns, name := "processfeed-dedup", "fg-dedup"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	fetchResult := oneEntryFetch()
	entryKey := computeEntryKey(fetchResult.Entries[0])
	fg.Status.LastSent[feed.RSSUrl] = map[string]string{entryKey: "2025-12-31T00:00:00Z"}
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
