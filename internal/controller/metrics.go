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
	labelOutcome   = "outcome"
	labelOperation = "operation"
)

// Outcome labels for feedOperationsTotal. One feed operation (fetch attempt
// or send attempt) is counted under exactly one of these per processFeed
// call, so an operator can see at a glance why a FeedGroup isn't posting
// without reading controller logs.
const (
	outcomeSent        = "sent"
	outcomeFetchError  = "fetch_error"
	outcomeSendError   = "send_error"
	outcomeRenderError = "render_error"
	outcomeRateLimited = "rate_limited"
)

// feedOperationsTotal counts feed fetch/send outcomes per FeedGroup, so
// `kubectl describe feedgroup` paired with this metric (or an alert on
// fetch_error/send_error rate) replaces having to grep controller logs to
// find out why a FeedGroup stopped posting.
var feedOperationsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rss2discord_feed_operations_total",
		Help: "Total number of RSS feed fetch/send operations processed, labeled by FeedGroup and outcome.",
	},
	[]string{labelNamespace, labelName, labelOutcome},
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
// host that has started hanging up to its timeout. Buckets span the client
// timeouts (RSS fetch 15s, Discord send 10s); observed on both success and
// failure, since the latency of a failing request is itself diagnostic.
//
// The classic Buckets are kept alongside the Native* fields so this exports
// as a hybrid histogram: existing consumers (the Grafana panels and heatmap
// querying ..._bucket/le) see no change, while a Prometheus server that
// negotiates protobuf and has native histograms enabled also gets the
// higher-resolution exponential representation for free.
var feedRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:                            "rss2discord_feed_request_duration_seconds",
		Help:                            "Duration of the operator's outbound HTTP requests, labeled by FeedGroup and operation (fetch/send).",
		Buckets:                         []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15},
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: time.Hour,
	},
	[]string{labelNamespace, labelName, labelOperation},
)

// feedLastSuccessTimestamp records the Unix time of the most recent
// successful Discord delivery for a FeedGroup, so freshness can be alerted on
// as `time() - rss2discord_feed_last_success_timestamp_seconds`. It only
// advances on an actual delivery, not on an empty/304 check, so it answers
// "when did this FeedGroup last *post* something" rather than "when was it
// last reconciled".
var feedLastSuccessTimestamp = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "rss2discord_feed_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful Discord delivery for this FeedGroup.",
	},
	[]string{labelNamespace, labelName},
)

// deleteFeedGroupMetrics drops every series for a FeedGroup that no longer
// exists. Without it a deleted FeedGroup's series linger in the registry
// until the controller restarts -- harmless for the counters, but actively
// misleading for feedLastSuccessTimestamp, which would otherwise report a
// frozen "last delivery" forever and trip any freshness alert built on it.
func deleteFeedGroupMetrics(namespace, name string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelName: name}
	feedOperationsTotal.DeletePartialMatch(labels)
	feedRequestDuration.DeletePartialMatch(labels)
	feedLastSuccessTimestamp.DeletePartialMatch(labels)
}

func init() {
	metrics.Registry.MustRegister(feedOperationsTotal, feedRequestDuration, feedLastSuccessTimestamp)
}
