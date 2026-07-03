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
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// newMetricsFeedGroup builds a minimal FeedGroup (status maps initialized)
// keyed by a unique namespace/name so each test's metric series starts from
// zero, avoiding cross-test accumulation in the shared registry.
func newMetricsFeedGroup(namespace, name, format string) (*v1alpha1.FeedGroup, v1alpha1.FeedSpec) {
	feed := v1alpha1.FeedSpec{RSSUrl: exampleFeedURL, Format: format}
	fg := &v1alpha1.FeedGroup{}
	fg.Namespace = namespace
	fg.Name = name
	fg.Spec.Feeds = []v1alpha1.FeedSpec{feed}
	ensureFeedStatuses(fg)
	return fg, feed
}

// oneEntryFetch is a fetch result carrying a single sendable entry.
func oneEntryFetch() rss.FetchResult {
	const articleURL = "https://example.com/article"
	return rss.FetchResult{Entries: []rss.Entry{{
		ID:          articleURL,
		Title:       "Example entry",
		Description: "Example entry body",
		Link:        articleURL,
	}}}
}

// histogramSampleCount reads the observation count of a single
// feedRequestDuration series.
func histogramSampleCount(t *testing.T, namespace, name, operation string) uint64 {
	t.Helper()
	obs, err := feedRequestDuration.GetMetricWithLabelValues(namespace, name, operation)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestProcessFeed_RecordsOutcomeMetric exercises each terminal path of
// processFeed and asserts feedOperationsTotal is incremented under the right
// outcome label -- the labels the dashboard and PrometheusRule alerts depend
// on, which previously had no test coverage at all.
func TestProcessFeed_RecordsOutcomeMetric(t *testing.T) {
	ctx := t.Context()

	cases := []struct {
		name     string
		outcome  string
		format   string
		withFeed bool
		fetchErr error
		setup    func(d *MockDiscordServer)
	}{
		{name: "sent", outcome: outcomeSent, withFeed: true},
		{name: "fetch_error", outcome: fetchErrorOutcome(classOther), fetchErr: errors.New("boom")},
		{
			name:     "send_error",
			outcome:  sendErrorOutcome(classServerError),
			withFeed: true,
			setup:    func(d *MockDiscordServer) { d.FailNextRequests(1, http.StatusInternalServerError) },
		},
		{
			name:     "rate_limited",
			outcome:  outcomeRateLimited,
			withFeed: true,
			setup:    func(d *MockDiscordServer) { d.FailNextRequestsRateLimited(1, "1") },
		},
		{
			// A template that references a field absent from the render data
			// parses fine but fails at execution, driving the render-error path.
			name:     "render_error",
			outcome:  outcomeRenderError,
			withFeed: true,
			format:   "{{.DoesNotExist}}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "metrics-"+tc.name, "fg-"+tc.name
			defer deleteFeedGroupMetrics(ns, name)

			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			if tc.setup != nil {
				tc.setup(discordServer)
			}

			fg, feed := newMetricsFeedGroup(ns, name, tc.format)
			client := discordServer.DiscordClientBuilder()(discordServer.URL())

			var fetchResult rss.FetchResult
			if tc.withFeed {
				fetchResult = oneEntryFetch()
			}
			(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, fetchResult, tc.fetchErr, client, "2026-01-01T00:00:00Z")

			if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, tc.outcome)); got != 1 {
				t.Fatalf("feedOperationsTotal{outcome=%q} = %v, want 1", tc.outcome, got)
			}
		})
	}
}

// TestProcessFeed_SentRecordsDurationAndFreshness asserts a successful
// delivery both observes the send-duration histogram and advances the
// last-success freshness gauge.
func TestProcessFeed_SentRecordsDurationAndFreshness(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-sent-extra", "fg-sent-extra"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got <= 0 {
		t.Fatalf("feedLastSuccessTimestamp = %v, want > 0", got)
	}
	if got := histogramSampleCount(t, ns, name, operationSend); got != 1 {
		t.Fatalf("send duration sample count = %d, want 1", got)
	}
}

// TestProcessFeed_QuietFetchAdvancesFreshness asserts that a successful
// check with nothing new to send -- a 304, or a fetch that returned zero
// entries -- still advances feedLastSuccessTimestamp. Before this, the gauge
// only moved on an actual Discord delivery, so a feed that's simply caught up
// (rather than broken) would eventually trip a staleness alert.
func TestProcessFeed_QuietFetchAdvancesFreshness(t *testing.T) {
	ctx := t.Context()

	cases := []struct {
		name        string
		fetchResult rss.FetchResult
	}{
		{name: "not_modified", fetchResult: rss.FetchResult{NotModified: true}},
		{name: "no_new_entries", fetchResult: rss.FetchResult{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name := "metrics-quiet-"+tc.name, "fg-quiet-"+tc.name
			defer deleteFeedGroupMetrics(ns, name)

			fg, feed := newMetricsFeedGroup(ns, name, "")
			(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, tc.fetchResult, nil, nil, "2026-01-01T00:00:00Z")

			if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got <= 0 {
				t.Fatalf("feedLastSuccessTimestamp = %v, want > 0", got)
			}
		})
	}
}

// messageOverflowSampleCount reads the observation count of a single
// messageOverflowChars series.
func messageOverflowSampleCount(t *testing.T, namespace, name string) uint64 {
	t.Helper()
	obs, err := messageOverflowChars.GetMetricWithLabelValues(namespace, name)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestProcessFeed_RecordsMessageOverflow asserts that sending an entry whose
// rendered content exceeds Discord's message length limit records an
// observation on messageOverflowChars, so an operator can see how often a
// FeedGroup's content is actually getting cut off.
func TestProcessFeed_RecordsMessageOverflow(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-overflow", "fg-overflow"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	const articleURL = "https://example.com/long-article"
	fetchResult := rss.FetchResult{Entries: []rss.Entry{{
		ID:          articleURL,
		Title:       "Example entry",
		Description: strings.Repeat("x", maxDiscordMessageLength+500),
		Link:        articleURL,
	}}}

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, fetchResult, nil, client, "2026-01-01T00:00:00Z")

	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want 1", got)
	}
}

// TestProcessFeed_NoOverflowMetricWhenContentFits asserts that sending an
// entry whose rendered content fits within Discord's limits does not record
// an observation, so the histogram's count reflects only actual overflows.
func TestProcessFeed_NoOverflowMetricWhenContentFits(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-no-overflow", "fg-no-overflow"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("messageOverflowChars sample count = %d, want 0", got)
	}
}

// TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp asserts
// that messageOverflowChars also counts truncation caused by Discord's
// combined embed length limit (title+description+footer+author <= 6000),
// which is enforced inside the discord package (clampEmbedTotalLength)
// rather than by the per-field truncation buildDiscordMessage measures
// directly. Title and Description here individually stay well under their
// own limits, so only the combined-length clamp can be responsible for any
// recorded overflow.
func TestProcessFeed_RecordsMessageOverflowFromEmbedTotalLengthClamp(t *testing.T) {
	ctx := t.Context()
	ns, name := "metrics-embed-total-overflow", "fg-embed-total-overflow"
	defer deleteFeedGroupMetrics(ns, name)

	discordServer := NewMockDiscordServer()
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "")
	feed.Embed = &v1alpha1.EmbedSpec{
		Enabled:    true,
		AuthorName: strings.Repeat("a", 3000),
		FooterText: strings.Repeat("f", 3500),
	}
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	(&FeedGroupReconciler{}).processFeed(ctx, fg, feed, oneEntryFetch(), nil, client, "2026-01-01T00:00:00Z")

	if got := messageOverflowSampleCount(t, ns, name); got != 1 {
		t.Fatalf("messageOverflowChars sample count = %d, want 1", got)
	}
}

// assertNativeOnlyHistogram fails the test unless m has a native histogram
// schema and no classic buckets, i.e. it isn't exposed twice (once as
// classic, once as native) for the same observations.
func assertNativeOnlyHistogram(t *testing.T, m *dto.Metric) {
	t.Helper()
	h := m.GetHistogram()
	if len(h.GetBucket()) != 0 {
		t.Fatalf("classic buckets present (%d), want none -- Buckets should be unset so this is native-only", len(h.GetBucket()))
	}
	if h.Schema == nil {
		t.Fatal("native histogram schema missing, want NativeHistogramBucketFactor to enable it")
	}
}

// TestFeedRequestDuration_NativeOnlyHistogram guards the native-only
// exposition config on feedRequestDuration: no classic buckets (so it isn't
// duplicated as a classic + native pair of dashboard panels) while the
// native histogram schema is populated.
func TestFeedRequestDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-native-only", "fg-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	feedRequestDuration.WithLabelValues(ns, name, operationFetch).Observe(0.2)

	obs, err := feedRequestDuration.GetMetricWithLabelValues(ns, name, operationFetch)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	assertNativeOnlyHistogram(t, &m)
}

// TestFeedGroupReconcileDuration_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for feedGroupReconcileDuration.
func TestFeedGroupReconcileDuration_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-reconcile-native-only", "fg-reconcile-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	feedGroupReconcileDuration.WithLabelValues(ns, name).Observe(0.2)

	obs, err := feedGroupReconcileDuration.GetMetricWithLabelValues(ns, name)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	assertNativeOnlyHistogram(t, &m)
}

// TestMessageOverflowChars_NativeOnlyHistogram mirrors
// TestFeedRequestDuration_NativeOnlyHistogram for messageOverflowChars.
func TestMessageOverflowChars_NativeOnlyHistogram(t *testing.T) {
	ns, name := "metrics-overflow-native-only", "fg-overflow-native-only"
	defer deleteFeedGroupMetrics(ns, name)

	messageOverflowChars.WithLabelValues(ns, name).Observe(42)

	obs, err := messageOverflowChars.GetMetricWithLabelValues(ns, name)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	assertNativeOnlyHistogram(t, &m)
}

// TestDeleteFeedGroupMetrics confirms a deleted FeedGroup's series are
// dropped, so a removed group can't leave a stale freshness reading behind.
func TestDeleteFeedGroupMetrics(t *testing.T) {
	ns, name := "metrics-delete", "fg-delete"

	feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent).Inc()
	feedRequestDuration.WithLabelValues(ns, name, operationSend).Observe(0.1)
	feedLastSuccessTimestamp.WithLabelValues(ns, name).Set(123)
	messageOverflowChars.WithLabelValues(ns, name).Observe(42)
	feedGroupReconcileDuration.WithLabelValues(ns, name).Observe(0.5)

	deleteFeedGroupMetrics(ns, name)

	// After deletion, re-fetching a series via WithLabelValues recreates it at
	// zero; a non-zero read would mean the original series was never removed.
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, exampleFeedURL, outcomeSent)); got != 0 {
		t.Fatalf("feedOperationsTotal after delete = %v, want 0", got)
	}
	if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got != 0 {
		t.Fatalf("feedLastSuccessTimestamp after delete = %v, want 0", got)
	}
	if got := histogramSampleCount(t, ns, name, operationSend); got != 0 {
		t.Fatalf("send duration sample count after delete = %d, want 0", got)
	}
	if got := messageOverflowSampleCount(t, ns, name); got != 0 {
		t.Fatalf("message overflow sample count after delete = %d, want 0", got)
	}
	if got := reconcileDurationSampleCount(t, ns, name); got != 0 {
		t.Fatalf("reconcile duration sample count after delete = %d, want 0", got)
	}
	deleteFeedGroupMetrics(ns, name)
}

// reconcileDurationSampleCount reads the observation count of a single
// feedGroupReconcileDuration series.
func reconcileDurationSampleCount(t *testing.T, namespace, name string) uint64 {
	t.Helper()
	count, err := reconcileDurationSampleCountErr(namespace, name)
	if err != nil {
		t.Fatalf("get histogram metric: %v", err)
	}
	return count
}

// reconcileDurationSampleCountErr is the error-returning core of
// reconcileDurationSampleCount, usable from contexts (like Ginkgo specs)
// that don't have a *testing.T to call Fatalf on.
func reconcileDurationSampleCountErr(namespace, name string) (uint64, error) {
	obs, err := feedGroupReconcileDuration.GetMetricWithLabelValues(namespace, name)
	if err != nil {
		return 0, err
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		return 0, err
	}
	return m.GetHistogram().GetSampleCount(), nil
}
