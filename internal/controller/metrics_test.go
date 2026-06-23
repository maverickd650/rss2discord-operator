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
	"errors"
	"net/http"
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
	(&FeedGroupReconciler{}).setDefaultStatusMaps(fg)
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
	ctx := context.Background()

	cases := []struct {
		name     string
		outcome  string
		format   string
		withFeed bool
		fetchErr error
		setup    func(d *MockDiscordServer)
	}{
		{name: "sent", outcome: outcomeSent, withFeed: true},
		{name: "fetch_error", outcome: outcomeFetchError, fetchErr: errors.New("boom")},
		{
			name:     "send_error",
			outcome:  outcomeSendError,
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

			if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, tc.outcome)); got != 1 {
				t.Fatalf("feedOperationsTotal{outcome=%q} = %v, want 1", tc.outcome, got)
			}
		})
	}
}

// TestProcessFeed_SentRecordsDurationAndFreshness asserts a successful
// delivery both observes the send-duration histogram and advances the
// last-success freshness gauge.
func TestProcessFeed_SentRecordsDurationAndFreshness(t *testing.T) {
	ctx := context.Background()
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
	ctx := context.Background()

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

// TestFeedRequestDuration_HybridHistogram guards the hybrid exposition
// config on feedRequestDuration: classic buckets must stay present (so the
// existing Grafana panels/heatmap keep working unchanged) while the native
// histogram fields are also populated (so a Prometheus server that enables
// native histograms benefits without any chart change).
func TestFeedRequestDuration_HybridHistogram(t *testing.T) {
	ns, name := "metrics-hybrid", "fg-hybrid"
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

	h := m.GetHistogram()
	if len(h.GetBucket()) == 0 {
		t.Fatal("classic buckets missing, want them retained for existing dashboard/heatmap queries")
	}
	if h.Schema == nil {
		t.Fatal("native histogram schema missing, want NativeHistogramBucketFactor to enable it")
	}
}

// TestDeleteFeedGroupMetrics confirms a deleted FeedGroup's series are
// dropped, so a removed group can't leave a stale freshness reading behind.
func TestDeleteFeedGroupMetrics(t *testing.T) {
	ns, name := "metrics-delete", "fg-delete"

	feedOperationsTotal.WithLabelValues(ns, name, outcomeSent).Inc()
	feedRequestDuration.WithLabelValues(ns, name, operationSend).Observe(0.1)
	feedLastSuccessTimestamp.WithLabelValues(ns, name).Set(123)

	deleteFeedGroupMetrics(ns, name)

	// After deletion, re-fetching a series via WithLabelValues recreates it at
	// zero; a non-zero read would mean the original series was never removed.
	if got := testutil.ToFloat64(feedOperationsTotal.WithLabelValues(ns, name, outcomeSent)); got != 0 {
		t.Fatalf("feedOperationsTotal after delete = %v, want 0", got)
	}
	if got := testutil.ToFloat64(feedLastSuccessTimestamp.WithLabelValues(ns, name)); got != 0 {
		t.Fatalf("feedLastSuccessTimestamp after delete = %v, want 0", got)
	}
	if got := histogramSampleCount(t, ns, name, operationSend); got != 0 {
		t.Fatalf("send duration sample count after delete = %d, want 0", got)
	}
	deleteFeedGroupMetrics(ns, name)
}
