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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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
	[]string{"namespace", "name", "outcome"},
)

func init() {
	metrics.Registry.MustRegister(feedOperationsTotal)
}
