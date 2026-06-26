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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metric label names, shared across the vectors below and
// deleteFeedGroupMetrics so the strings live in exactly one place.
const (
	labelNamespace = "namespace"
	labelName      = "name"
	labelFeedURL   = "rss_url"
	labelOutcome   = "outcome"
	labelOperation = "operation"
)

// Outcome labels for feedOperationsTotal. One feed operation (fetch attempt
// or send attempt) is counted under exactly one of these per processFeed
// call, so an operator can see at a glance why a FeedGroup isn't posting
// without reading controller logs.
//
// fetch_error and send_error are further split by failureClass.metricReason
// (see classify.go) into fetch_error_<reason>/send_error_<reason> -- e.g.
// fetch_error_not_found vs. fetch_error_timeout -- so a feed stuck on a
// permanent 404 shows up as a distinct series rather than being
// indistinguishable from one flapping on timeouts. outcomeFetchError and
// outcomeSendError themselves are never recorded as an outcome value
// directly, only used as the label prefix; existing dashboards/alerts that
// matched the exact "fetch_error"/"send_error" values were updated to
// "fetch_error.*"/"send_error.*" since Prometheus label regex matches are
// fully anchored (see dist/chart).
const (
	outcomeSent        = "sent"
	outcomeFetchError  = "fetch_error"
	outcomeSendError   = "send_error"
	outcomeRenderError = "render_error"
	outcomeRateLimited = "rate_limited"
)

// fetchErrorOutcome and sendErrorOutcome build the diversified outcome label
// for a classified fetch/send failure, e.g. "fetch_error_not_found".
func fetchErrorOutcome(class failureClass) string {
	return outcomeFetchError + "_" + class.metricReason
}

func sendErrorOutcome(class failureClass) string {
	return outcomeSendError + "_" + class.metricReason
}

// feedOperationsTotal counts feed fetch/send outcomes per FeedGroup, so
// `kubectl describe feedgroup` paired with this metric (or an alert on
// fetch_error/send_error rate) replaces having to grep controller logs to
// find out why a FeedGroup stopped posting. labelFeedURL identifies which
// feed within the group an outcome belongs to -- a FeedGroup can have up to
// 50 feeds (FeedGroupSpec.Feeds), so namespace/name alone can't tell an
// operator which one is failing without a status dive; PrometheusRule alerts
// surface it directly via $labels.rss_url.
var feedOperationsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rss2discord_feed_operations_total",
		Help: "Total number of RSS feed fetch/send operations processed, labeled by FeedGroup, feed URL, and outcome.",
	},
	[]string{labelNamespace, labelName, labelFeedURL, labelOutcome},
)

// operation labels for feedRequestDuration, distinguishing the two outbound
// HTTP calls a reconcile makes per feed.
const (
	operationFetch = "fetch"
	operationSend  = "send"
)

// feedRequestDuration records how long the operator's outbound HTTP calls
// take, split by operation (the RSS fetch vs the Discord send), so the
// dashboard can surface slow feeds/webhooks and an alert can catch a feed
// host that has started hanging up to its timeout. Observed on both success
// and failure, since the latency of a failing request is itself diagnostic.
//
// Buckets is deliberately left unset: with NativeHistogramBucketFactor set
// and no classic Buckets, client_golang exports this as a native-only
// histogram (see prometheus.NewHistogram), so there's a single series per
// label set instead of a classic/native pair the dashboard would otherwise
// have to show twice.
var feedRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:                            "rss2discord_feed_request_duration_seconds",
		Help:                            "Duration of the operator's outbound HTTP requests, labeled by FeedGroup and operation (fetch/send).",
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	},
	[]string{labelNamespace, labelName, labelOperation},
)

// feedLastSuccessTimestamp records the Unix time of the most recent
// successful check of any feed in a FeedGroup, so freshness can be alerted on
// as `time() - rss2discord_feed_last_success_timestamp_seconds`. It advances
// on any successful fetch -- including a 304 or a fetch with nothing new --
// not just one that resulted in a Discord delivery, so a quiet-but-healthy
// feed doesn't look stale just because it has nothing new to post.
var feedLastSuccessTimestamp = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "rss2discord_feed_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful check of this FeedGroup's feeds.",
	},
	[]string{labelNamespace, labelName},
)

// messageOverflowChars records how many characters were trimmed from a
// rendered Discord message/embed-description/thread-name before it was
// clamped to fit Discord's length limits (truncateMessage in
// feedgroup_controller.go). Only actual overflows are observed -- the vast
// majority of entries render under the limit -- so histogram_count(rate(...))
// directly answers "how often does this FeedGroup's content get cut off,"
// rather than burying that signal in a sea of zero observations.
//
// Like feedRequestDuration, Buckets is left unset so this exports as a
// native-only histogram rather than a classic/native pair.
var messageOverflowChars = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:                            "rss2discord_message_overflow_chars",
		Help:                            "Characters trimmed from a rendered Discord message before it was clamped to fit Discord's length limits. Only recorded when content actually overflowed.",
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	},
	[]string{labelNamespace, labelName},
)

// feedGroupReconcileDuration records the wall-clock time of a full Reconcile
// call (RSS fetch, send, and status write across every feed in the group),
// so a FeedGroup whose reconciles are creeping toward the requeue interval
// shows up before it starts missing its interval altogether.
// controller-runtime's own controller_runtime_reconcile_time_seconds covers
// this at the per-controller level but is classic-only and not labeled by
// FeedGroup, so this is the value-add: a native-only series (Buckets left
// unset, see feedRequestDuration) an operator can filter to one misbehaving
// group.
var feedGroupReconcileDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:                            "rss2discord_feedgroup_reconcile_duration_seconds",
		Help:                            "Duration of a full FeedGroup reconcile, covering every feed's fetch and send.",
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	},
	[]string{labelNamespace, labelName},
)

// deleteFeedGroupMetrics drops every series for a FeedGroup that no longer
// exists. Without it a deleted FeedGroup's series linger in the registry
// until the controller restarts -- harmless for the counters, but actively
// misleading for feedLastSuccessTimestamp, which would otherwise report a
// frozen "last checked" forever and trip any freshness alert built on it.
func deleteFeedGroupMetrics(namespace, name string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelName: name}
	feedOperationsTotal.DeletePartialMatch(labels)
	feedRequestDuration.DeletePartialMatch(labels)
	feedLastSuccessTimestamp.DeletePartialMatch(labels)
	messageOverflowChars.DeletePartialMatch(labels)
	feedGroupReconcileDuration.DeletePartialMatch(labels)
}

func init() {
	metrics.Registry.MustRegister(
		feedOperationsTotal,
		feedRequestDuration,
		feedLastSuccessTimestamp,
		messageOverflowChars,
		feedGroupReconcileDuration,
	)
}
