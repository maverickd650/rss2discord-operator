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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rss2discordv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const (
	secretURLKey             = "url"
	defaultInterval          = "30m"
	feedGroupNameBasic       = "test-feedgroup"
	feedGroupNameNoSecret    = "test-feedgroup-no-secret"
	feedGroupNameRetry       = "test-feedgroup-retry"
	feedGroupNameFilter      = "test-feedgroup-filter"
	feedGroupNameKeywords    = "test-feedgroup-keywords"
	feedGroupNamePaused      = "test-feedgroup-paused"
	feedGroupNameTimestamp   = "test-feedgroup-timestamp"
	exampleFeedURL           = "https://example.com/feed.xml"
	discordWebhookSecretName = "discord-webhook"
)

// testRSSClient is a plain (non-SSRF-guarded) RSS client used in tests so
// that reconciliation can reach mock servers bound to loopback addresses.
// Production code goes through rss.NewClient(nil), which guards against
// connecting to non-public addresses.
func testRSSClient() *rss.Client {
	return rss.NewClient(&http.Client{})
}

// feedConditionReason returns the Reason of url's condType condition within
// fg.Status.Feeds, or "" if either the feed or that condition isn't present.
// A handful of envtest assertions below want to check the classified Reason
// (e.g. "HTTP404") on a feed's Reachable/Delivered condition without
// repeating the find-feed-then-find-condition boilerplate at every call
// site.
func feedConditionReason(fg *rss2discordv1alpha1.FeedGroup, url, condType string) string {
	fs := feedStatusFor(fg, url)
	if fs == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(fs.Conditions, condType)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

// feedsWithError counts how many of fg's feeds currently have a non-empty
// LastError, the slice-based equivalent of the old `len(Status.LastError)`
// map-length check.
func feedsWithError(fg *rss2discordv1alpha1.FeedGroup) int {
	count := 0
	for _, fs := range fg.Status.Feeds {
		if fs.LastError != "" {
			count++
		}
	}
	return count
}

// MockRSSServer provides a mock RSS feed for testing
type MockRSSServer struct {
	server          *httptest.Server
	feedContent     string
	statusCode      int
	etag            string
	requestCount    int
	ifNoneMatchSeen []string
}

// NewMockRSSServer creates a new mock RSS server with test feed data
func NewMockRSSServer(feedContent string) *MockRSSServer {
	m := &MockRSSServer{
		feedContent: feedContent,
		statusCode:  http.StatusOK,
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		m.requestCount++
		ifNoneMatch := r.Header.Get("If-None-Match")
		m.ifNoneMatchSeen = append(m.ifNoneMatchSeen, ifNoneMatch)

		if m.etag != "" {
			w.Header().Set("ETag", m.etag)
			if ifNoneMatch == m.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.WriteHeader(m.statusCode)
		_, _ = w.Write([]byte(m.feedContent))
	}))

	return m
}

// SetETag makes the server emit the given ETag header and respond 304 to
// any request whose If-None-Match matches it.
func (m *MockRSSServer) SetETag(etag string) {
	m.etag = etag
}

// SetFeedContent replaces the feed body returned by subsequent requests, so
// a test can simulate a feed's contents changing between two reconciles.
func (m *MockRSSServer) SetFeedContent(feedContent string) {
	m.feedContent = feedContent
}

// RequestCount returns how many GET requests the server has received.
func (m *MockRSSServer) RequestCount() int {
	return m.requestCount
}

// IfNoneMatchSeen returns the If-None-Match header value from each request
// received so far, in arrival order (empty string if not sent).
func (m *MockRSSServer) IfNoneMatchSeen() []string {
	return m.ifNoneMatchSeen
}

func (m *MockRSSServer) URL() string {
	return m.server.URL
}

func (m *MockRSSServer) Close() {
	m.server.Close()
}

// MockDiscordServer provides a mock Discord webhook for testing
type MockDiscordServer struct {
	server           *httptest.Server
	messagesReceived int
	deliveryAttempts int
	bodiesReceived   []string
	failNext         int
	failStatus       int
	failRetryAfter   string
}

// NewMockDiscordServer creates a new mock Discord server. It uses TLS and
// registers its loopback host with discord.AllowedWebhookHosts so it can
// stand in for a real discord.com webhook, since the production client
// rejects non-Discord hosts (SSRF/domain-confusion guard).
func NewMockDiscordServer() *MockDiscordServer {
	d := &MockDiscordServer{
		messagesReceived: 0,
	}

	d.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// deliveryAttempts counts every POST the controller actually made,
		// including ones that fail, so tests can assert the controller stopped
		// sending (e.g. after a 429) rather than only how many succeeded.
		d.deliveryAttempts++

		if d.failNext > 0 {
			d.failNext--
			if d.failRetryAfter != "" {
				w.Header().Set("Retry-After", d.failRetryAfter)
			}
			w.WriteHeader(d.failStatus)
			return
		}

		body, _ := io.ReadAll(r.Body)
		d.messagesReceived++
		d.bodiesReceived = append(d.bodiesReceived, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))

	discord.AllowedWebhookHosts[d.server.Listener.Addr().(*net.TCPAddr).IP.String()] = true

	return d
}

// FailNextRequests makes the next n webhook deliveries fail with statusCode
// instead of succeeding, so tests can exercise send-failure/retry paths.
func (d *MockDiscordServer) FailNextRequests(n int, statusCode int) {
	d.failNext = n
	d.failStatus = statusCode
}

// FailNextRequestsRateLimited makes the next n webhook deliveries return a
// 429 with the given Retry-After header, so tests can exercise the
// controller's rate-limit backoff path.
func (d *MockDiscordServer) FailNextRequestsRateLimited(n int, retryAfter string) {
	d.failNext = n
	d.failStatus = http.StatusTooManyRequests
	d.failRetryAfter = retryAfter
}

// Bodies returns the raw request bodies received so far, in arrival order.
func (d *MockDiscordServer) Bodies() []string {
	return d.bodiesReceived
}

func (d *MockDiscordServer) URL() string {
	return d.server.URL
}

func (d *MockDiscordServer) Close() {
	delete(discord.AllowedWebhookHosts, d.server.Listener.Addr().(*net.TCPAddr).IP.String())
	d.server.Close()
}

func (d *MockDiscordServer) MessageCount() int {
	return d.messagesReceived
}

// DeliveryAttempts returns how many webhook POSTs the controller made,
// counting failed deliveries (e.g. 429s) as well as successful ones.
func (d *MockDiscordServer) DeliveryAttempts() int {
	return d.deliveryAttempts
}

// DiscordClientBuilder returns a DiscordClientBuilder that trusts this mock
// server's self-signed TLS certificate.
func (d *MockDiscordServer) DiscordClientBuilder() func(webhookURL string) *discord.Client {
	client := d.server.Client()
	return func(webhookURL string) *discord.Client {
		return discord.NewClientWithHTTP(webhookURL, client)
	}
}

// Helper function to create an RSS feed XML
func createRSSFeed(entries ...struct {
	title       string
	description string
	link        string
	pubDate     string
	guid        string
}) string {
	var feed strings.Builder
	feed.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
`)

	for _, entry := range entries {
		fmt.Fprintf(&feed, `    <item>
      <title>%s</title>
      <description>%s</description>
      <link>%s</link>
      <pubDate>%s</pubDate>
      <guid>%s</guid>
    </item>
`, entry.title, entry.description, entry.link, entry.pubDate, entry.guid)
	}

	feed.WriteString(`  </channel>
</rss>`)
	return feed.String()
}

var _ = Describe("FeedGroup Controller", func() {
	const (
		namespace = "default"
	)

	var (
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create namespace if it doesn't exist
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		}
	})

	Describe("Successful RSS to Discord flow", func() {
		It("should fetch RSS entries and track status", func() {
			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Test Article 1",
					description: "This is a test article",
					link:        "https://example.com/article1",
					pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
					guid:        "article-1",
				},
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Test Article 2",
					description: "This is another test article",
					link:        "https://example.com/article2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "article-2",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discordWebhookSecretName,
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameBasic,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Format:        "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameBasic, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameBasic, Namespace: namespace}, updated)).To(Succeed())

			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())

			By("Verifying no errors in status")
			Expect(feedsWithError(updated)).To(Equal(0))

			By("Verifying the Ready condition reflects success")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
		})
	})

	Describe("Conditional RSS fetches", func() {
		It("should send a stored ETag on the next reconcile and skip re-processing on 304", func() {
			const feedGroupName = "test-feedgroup-conditional-get"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Conditional Article",
					description: "First fetch",
					link:        "https://example.com/conditional-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "conditional-article",
				},
			))
			rssServer.SetETag(`"v1"`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-conditional",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-conditional"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running the first reconciliation")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(rssServer.IfNoneMatchSeen()).To(Equal([]string{""}))

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).To(Equal(`"v1"`))
			lastSeenAfterFirst := feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry
			Expect(lastSeenAfterFirst).NotTo(BeEmpty())
			resourceVersionAfterFirst := afterFirst.ResourceVersion

			By("Running a second reconciliation against the unchanged feed")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the second request carried the stored ETag and got a 304")
			Expect(rssServer.RequestCount()).To(Equal(2))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(Equal(`"v1"`))

			By("Verifying no new message was sent and status was left untouched")
			Expect(discordServer.MessageCount()).To(Equal(1))

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).To(Equal(lastSeenAfterFirst))
			Expect(feedsWithError(afterSecond)).To(Equal(0))
			Expect(feedStatusFor(afterSecond, rssServer.URL()).RetryCount).To(Equal(0))

			By("Verifying the unchanged-status reconcile skipped the status write entirely")
			Expect(afterSecond.ResourceVersion).To(Equal(resourceVersionAfterFirst))
		})
	})

	Describe("Conditional GET validators after a send failure", func() {
		It("should not persist the new ETag until the entry is actually sent", func() {
			const feedGroupName = "test-feedgroup-conditional-get-retry"

			By("Creating mock Discord webhook server that fails the first delivery")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			discordServer.FailNextRequests(1, http.StatusInternalServerError)

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Retry Article",
					description: "Should survive a failed send",
					link:        "https://example.com/retry-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "retry-article",
				},
			))
			rssServer.SetETag(`"v1"`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-conditional-retry",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-conditional-retry"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running the first reconciliation, where the Discord send fails")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the new ETag was NOT persisted, since the entry was never sent")
			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastError).NotTo(BeEmpty())

			By("Running a second reconciliation, where the Discord send succeeds")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the previously unsent entry was retried and delivered")
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(BeEmpty(), "second fetch should not have sent the never-persisted ETag")

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).ETag).To(Equal(`"v1"`))
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
			Expect(feedsWithError(afterSecond)).To(Equal(0))
		})
	})

	Describe("LastSeenEntry no longer present in the feed", func() {
		It("should fall back to catch-up instead of silently skipping every entry forever", func() {
			const feedGroupName = "test-feedgroup-stale-lastseen"

			type item = struct {
				title       string
				description string
				link        string
				pubDate     string
				guid        string
			}

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with a single entry")
			rssServer := NewMockRSSServer(createRSSFeed(item{
				title:       "Original Article",
				description: "The only entry in the feed's initial window",
				link:        "https://example.com/original-article",
				pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
				guid:        "original-article",
			}))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-stale-lastseen",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-stale-lastseen"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					CatchUpLimit:  5,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running the first reconciliation, which sends and records the original entry")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry).To(Equal("original-article"))

			By("Replacing the feed's window so the recorded entry is no longer present at all")
			rssServer.SetFeedContent(createRSSFeed(
				item{
					title:       "Replacement Article 1",
					description: "Published after the original scrolled out of the feed's window",
					link:        "https://example.com/replacement-1",
					pubDate:     time.Now().Add(-30 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-1",
				},
				item{
					title:       "Replacement Article 2",
					description: "Also new",
					link:        "https://example.com/replacement-2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "replacement-2",
				},
			))

			By("Running a second reconciliation against the changed feed")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying both new entries were sent instead of being silently skipped forever")
			Expect(discordServer.MessageCount()).To(Equal(3))

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).To(Equal("replacement-2"))

			By("Adding one more entry while keeping the now-recorded entry present")
			rssServer.SetFeedContent(createRSSFeed(
				item{
					title:       "Replacement Article 1",
					description: "Published after the original scrolled out of the feed's window",
					link:        "https://example.com/replacement-1",
					pubDate:     time.Now().Add(-30 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-1",
				},
				item{
					title:       "Replacement Article 2",
					description: "Also new",
					link:        "https://example.com/replacement-2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "replacement-2",
				},
				item{
					title:       "Replacement Article 3",
					description: "Newest entry, published after replacement-2",
					link:        "https://example.com/replacement-3",
					pubDate:     time.Now().Add(1 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-3",
				},
			))

			By("Running a third reconciliation against the feed with the recorded entry still present")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the genuinely new entry was sent, since the recorded one was found in the feed")
			Expect(discordServer.MessageCount()).To(Equal(4))

			afterThird := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterThird)).To(Succeed())
			Expect(feedStatusFor(afterThird, rssServer.URL()).LastSeenEntry).To(Equal("replacement-3"))
		})
	})

	Describe("Catch-up limit", func() {
		It("should cap how many backlog entries are sent on first reconcile", func() {
			const feedGroupName = "test-feedgroup-catchup"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with a long backlog")
			type item = struct {
				title       string
				description string
				link        string
				pubDate     string
				guid        string
			}
			entries := make([]item, 0, 10)
			for i := range 10 {
				entries = append(entries, item{
					title:       fmt.Sprintf("Backlog Article %d", i),
					description: "An old article",
					link:        fmt.Sprintf("https://example.com/article%d", i),
					pubDate:     time.Now().Add(-time.Duration(10-i) * time.Hour).Format(time.RFC1123Z),
					guid:        fmt.Sprintf("backlog-%d", i),
				})
			}
			rssServer := NewMockRSSServer(createRSSFeed(entries...))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-catchup",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource with a catch-up limit of 3")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-catchup"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					CatchUpLimit:  3,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the configured catch-up limit was sent")
			Expect(discordServer.MessageCount()).To(Equal(3))
		})

		It("should deliver newest entries and advance the watermark for feeds without publish dates", func() {
			const feedGroupName = "test-feedgroup-nodate"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			type item = struct {
				title       string
				description string
				link        string
				pubDate     string
				guid        string
			}
			noDate := func(title, guid string) item {
				// pubDate is left empty on purpose: this feed has no publish
				// dates, which is exactly the case the ordering fallback covers.
				return item{title: title, description: "body", link: "https://example.com/" + guid, pubDate: "", guid: guid}
			}

			By("Creating a mock RSS feed (newest-first, no pubDate) with a 3-entry backlog")
			rssServer := NewMockRSSServer(createRSSFeed(
				noDate("Article C", "c"), // index 0 = newest by feed convention
				noDate("Article B", "b"),
				noDate("Article A", "a"), // index 2 = oldest
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-nodate",
					Namespace: namespace,
				},
				Data: map[string][]byte{secretURLKey: []byte(discordServer.URL())},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource with a catch-up limit of 2")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: feedGroupName, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-nodate"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					CatchUpLimit:  2,
					Feeds:         []rss2discordv1alpha1.FeedSpec{{RSSUrl: rssServer.URL()}},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running the first reconcile")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying catch-up sent the two NEWEST entries (B and C), not the oldest (A)")
			Expect(discordServer.MessageCount()).To(Equal(2))
			bodies := strings.Join(discordServer.Bodies(), "\n")
			Expect(bodies).To(ContainSubstring("Article B"))
			Expect(bodies).To(ContainSubstring("Article C"))
			Expect(bodies).NotTo(ContainSubstring("Article A"))

			By("Publishing a newer entry at the top of the still-dateless feed")
			rssServer.SetFeedContent(createRSSFeed(
				noDate("Article D", "d"), // brand new, now newest
				noDate("Article C", "c"),
				noDate("Article B", "b"),
				noDate("Article A", "a"),
			))

			By("Running the second reconcile")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the new entry D was delivered (watermark advanced, no silence, no re-send)")
			Expect(discordServer.MessageCount()).To(Equal(3))
			Expect(discordServer.Bodies()[2]).To(ContainSubstring("Article D"))
		})
	})

	Describe("Discord message rendering", func() {
		It("should strip HTML from entry descriptions", func() {
			const feedGroupName = "test-feedgroup-html"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with an HTML description")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "HTML Article",
					description: "&lt;p&gt;First paragraph.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;One&lt;/li&gt;&lt;/ul&gt;",
					link:        "https://example.com/html-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "html-article",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-html",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-html"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Format:        "{{.Description}}",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the sent message has no HTML tags")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			Expect(discordServer.Bodies()[0]).NotTo(ContainSubstring("<p>"))
			Expect(discordServer.Bodies()[0]).NotTo(ContainSubstring("<li>"))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("First paragraph."))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("One"))
		})

		It("should render an embed with color, thumbnail, and forum thread name when configured", func() {
			const feedGroupName = "test-feedgroup-embed"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with an enclosure image")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Embed Article</title>
      <description>Embed body text</description>
      <link>https://example.com/embed-article</link>
      <guid>embed-article</guid>
      <enclosure url="https://example.com/pic.jpg" type="image/jpeg" />
    </item>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-embed",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource with embed and forum thread config")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-embed"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Embed: &rss2discordv1alpha1.EmbedSpec{
						Enabled: true,
						Color:   "#00FF00",
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL(), ForumThreadName: "{{.Title}}"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the sent message is an embed with the configured color, thumbnail, and thread name")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			body := discordServer.Bodies()[0]
			Expect(body).To(ContainSubstring(`"title":"Embed Article"`))
			Expect(body).To(ContainSubstring(`"color":65280`))
			Expect(body).To(ContainSubstring(`"thumbnail":{"url":"https://example.com/pic.jpg"}`))
			Expect(body).To(ContainSubstring(`"thread_name":"Embed Article"`))
		})

		It("should drop a non-http(s) entry link/image instead of forwarding it into the embed", func() {
			const feedGroupName = "test-feedgroup-embed-unsafe-url"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server whose entry link/enclosure use a non-http(s) scheme")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Unsafe URL Article</title>
      <description>Body</description>
      <link>javascript:alert(1)</link>
      <guid>unsafe-url-article</guid>
      <enclosure url="data:image/png;base64,AAAA" type="image/png" />
    </item>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-embed-unsafe-url",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource with embed enabled")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-embed-unsafe-url"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Embed: &rss2discordv1alpha1.EmbedSpec{
						Enabled: true,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the embed's URL and thumbnail were dropped rather than carrying the unsafe scheme through")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			body := discordServer.Bodies()[0]
			Expect(body).NotTo(ContainSubstring("javascript:"))
			Expect(body).NotTo(ContainSubstring("data:image"))
			Expect(body).NotTo(ContainSubstring(`"thumbnail"`))
		})
	})

	Describe("Error handling", func() {
		It("should handle missing Discord webhook secret", func() {
			By("Creating FeedGroup without secret")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameNoSecret,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "nonexistent-secret"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: "https://example.com/feed",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameNoSecret, Namespace: namespace},
			})
			// Controller handles missing secret gracefully and requeues
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup still exists and status is updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameNoSecret, Namespace: namespace}, updated)).To(Succeed())
		})

		It("should handle a webhook secret whose value is blank", func() {
			const feedGroupName = "test-feedgroup-blank-secret-value"

			By("Creating a secret whose key holds only whitespace")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-blank-value",
					Namespace: namespace,
				},
				Data: map[string][]byte{secretURLKey: []byte("   ")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: feedGroupName, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-blank-value"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds:    []rss2discordv1alpha1.FeedSpec{{RSSUrl: exampleFeedURL}},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the webhook error was recorded and the Ready condition reflects it")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("WebhookUnresolved"))
			Expect(readyCondition.Message).To(ContainSubstring("empty"))
		})

		It("should handle a webhook secret missing the configured key", func() {
			const feedGroupName = "test-feedgroup-secret-missing-key"

			By("Creating a secret that does not contain the configured key")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-missing-key",
					Namespace: namespace,
				},
				Data: map[string][]byte{"wrong-key": []byte("https://discord.com/api/webhooks/12345/abcde")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: feedGroupName, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-missing-key"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds:    []rss2discordv1alpha1.FeedSpec{{RSSUrl: exampleFeedURL}},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the missing-key error was recorded on status")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Reason).To(Equal("WebhookUnresolved"))
			Expect(readyCondition.Message).To(ContainSubstring("missing key"))
		})
	})

	Describe("RSS error handling", func() {
		It("should handle RSS fetch errors with retries", func() {
			By("Creating a mock RSS feed server that returns an error")
			rssServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("Internal Server Error"))
			}))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-2",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameRetry,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-2"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "1m",
					Retries:       2,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
				Recorder:  recorder,
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			// Should requeue with retry interval instead of normal interval
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying error is tracked")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).LastError).NotTo(BeEmpty())

			By("Verifying the Ready condition reflects the failure")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("FeedErrors"))
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(1))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(2))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("FetchFailed")))

			By("Verifying the FeedReachable condition and classified error reason")
			Expect(feedConditionReason(updated, rssServer.URL, rss2discordv1alpha1.FeedConditionTypeReachable)).To(Equal("ServerError"))
			reachableCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeFeedReachable)
			Expect(reachableCondition).NotTo(BeNil())
			Expect(reachableCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachableCondition.Reason).To(Equal("ServerError"))

			By("Reconciling a third time, with the feed still failing the same way")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(3))
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should retry then give up on a deterministically failing render, instead of retrying forever", func() {
			const feedGroupName = "test-feedgroup-render-error"

			By("Creating a mock RSS feed server with one entry")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Render Error Article</title>
      <description>Body</description>
      <link>https://example.com/render-error-article</link>
      <guid>render-error-article</guid>
    </item>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-render-error",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with a template field that fails at execution time, not parse time")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-render-error"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "1m",
					Retries:       2,
					// {{.Bogus}} parses fine (the parser doesn't know field
					// names) but fails on every Execute, since Title/
					// Description/Link/Published are the only fields the
					// render data struct has.
					Format: "{{.Bogus}}",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
				Recorder:  recorder,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the render error is tracked and counted toward retries")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(1))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted, instead of retrying forever")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(2))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("RenderFailed")))

			By("Reconciling a third time, with the entry still failing to render the same way")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(3))
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should retry then give up on a persistently failing send, instead of retrying forever", func() {
			const feedGroupName = "test-feedgroup-send-error"

			By("Creating a mock Discord server that always fails the send with a non-rate-limit error")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			discordServer.FailNextRequests(3, http.StatusInternalServerError)

			By("Creating a mock RSS feed server with one entry")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Send Error Article",
					description: "Should never be delivered",
					link:        "https://example.com/send-error-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "send-error-article",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-send-error",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-send-error"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "1m",
					Retries:       2,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
				Recorder:             recorder,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the send error is tracked and counted toward retries")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(1))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted, instead of retrying forever")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(2))
			Expect(discordServer.MessageCount()).To(Equal(0))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("SendFailed")))

			By("Reconciling a third time, with the send still failing the same way")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(3))
			Consistently(recorder.Events).ShouldNot(Receive())
		})
	})

	Describe("Filtering", func() {
		It("should apply regex filters to RSS entries", func() {
			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with multiple entries")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Kubernetes Release v1.30",
					description: "New Kubernetes version released",
					link:        "https://example.com/k8s",
					pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
					guid:        "k8s-1",
				},
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Python Conference Schedule",
					description: "Python conference talks announced",
					link:        "https://example.com/python",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "python-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-3",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with regex filter")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameFilter,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-3"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
							Filter: &rss2discordv1alpha1.Filter{
								Regex: "Kubernetes|K8s",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameFilter, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameFilter, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
		})

		It("should apply keyword filters to RSS entries", func() {
			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server with multiple entries")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Python Tutorial",
					description: "Learn Python basics",
					link:        "https://example.com/python",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "python-1",
				},
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "JavaScript Best Practices",
					description: "Advanced JS patterns",
					link:        "https://example.com/js",
					pubDate:     time.Now().Add(time.Hour).Format(time.RFC1123Z),
					guid:        "js-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-4",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with keyword filter")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameKeywords,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-4"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
							Filter: &rss2discordv1alpha1.Filter{
								Keywords: []string{"Python", "Django"},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameKeywords, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameKeywords, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
		})
	})

	Describe("Paused feeds and status tracking", func() {
		It("should skip paused feeds", func() {
			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Test",
					description: "Test",
					link:        "https://example.com/test",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "test-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-5",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with paused feed")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNamePaused,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-5"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
							Paused: true,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNamePaused, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying paused feed was not processed")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNamePaused, Namespace: namespace}, updated)).To(Succeed())

			// LastSeenEntry should not be updated for paused feed
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty())
		})

		It("should track lastChecked timestamp for each feed", func() {
			By("Creating a mock RSS feed server with empty feed")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Empty Feed</title>
    <link>https://example.com</link>
    <description>Empty Feed</description>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-6",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupNameTimestamp,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-6"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameTimestamp, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying lastChecked was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameTimestamp, Namespace: namespace}, updated)).To(Succeed())

			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			// Should be a valid RFC3339 timestamp
			_, err = time.Parse(time.RFC3339, feedStatusFor(updated, rssServer.URL()).LastChecked)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip a feed whose permanent-failure backoff has not yet elapsed", func() {
			const feedGroupName = "test-feedgroup-backoff-skip"

			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Backoff Skip Article",
					description: "Backoff Skip Article",
					link:        "https://example.com/test",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "test-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-backoff-skip",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-backoff-skip"},
						Key:                  secretURLKey,
					},
					Interval: defaultInterval,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			// The first reconcile after creation has Generation > ObservedGeneration,
			// which clears any BackoffUntil set below it. Reconcile once first so
			// ObservedGeneration catches up, then set BackoffUntil and reconcile
			// again at the same generation, where the skip in Reconcile (the loop
			// building activeFeeds) actually applies.
			By("Running an initial reconciliation to advance ObservedGeneration")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rssServer.RequestCount()).To(Equal(1))

			By("Setting a future BackoffUntil on the feed's status")
			afterFirstReconcile := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirstReconcile)).To(Succeed())
			feedStatusFor(afterFirstReconcile, rssServer.URL()).BackoffUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			Expect(k8sClient.Status().Update(ctx, afterFirstReconcile)).To(Succeed())

			By("Running reconciliation")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the backed-off feed was not fetched again")
			Expect(rssServer.RequestCount()).To(Equal(1))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).To(Equal(feedStatusFor(afterFirstReconcile, rssServer.URL()).LastChecked))
		})
	})

	Describe("Status pruning for removed feeds", func() {
		It("should drop per-feed status entries once a feed is removed from spec", func() {
			const feedGroupName = "test-feedgroup-removed-feed"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Soon Removed",
					description: "This feed will be removed from spec",
					link:        "https://example.com/removed",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "removed-1",
				},
			))
			rssServer.SetETag(`"v1"`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-removed",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			const keptFeedURL = "https://example.com/kept-feed.xml"

			By("Creating FeedGroup resource with two feeds")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-removed"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Retries:       3,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
						{RSSUrl: keptFeedURL, Paused: true},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running the first reconciliation so the removed feed accrues status")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).NotTo(BeEmpty())

			By("Removing the feed from spec, keeping the paused one")
			afterFirst.Spec.Feeds = []rss2discordv1alpha1.FeedSpec{
				{RSSUrl: keptFeedURL, Paused: true},
			}
			Expect(k8sClient.Update(ctx, afterFirst)).To(Succeed())

			By("Running another reconciliation")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the removed feed's status entries were pruned")
			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL())).To(BeNil())
		})
	})

	Describe("Requeue behavior", func() {
		It("should use normal interval when no retries needed", func() {
			By("Creating a mock RSS feed server with empty feed")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Empty Feed</title>
    <link>https://example.com</link>
    <description>Empty Feed</description>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-7",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with custom interval")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-interval",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-7"},
						Key:                  secretURLKey,
					},
					Interval:      "1h",
					RetryInterval: "5m",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{
							RSSUrl: rssServer.URL(),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-feedgroup-interval", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should use normal interval (1 hour)
			expectedInterval := time.Hour
			Expect(result.RequeueAfter).To(Equal(expectedInterval))
		})
	})

	Describe("Spec validation", func() {
		It("should reject an Interval that isn't a valid Go duration at apply time", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-bad-interval",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-bad-interval",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-bad-interval"},
						Key:                  secretURLKey,
					},
					Interval: "30 minutes",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.interval")))
		})

		It("should reject a RetryInterval that isn't a valid Go duration at apply time", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-bad-retry-interval",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-bad-retry-interval",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-bad-retry-interval"},
						Key:                  secretURLKey,
					},
					RetryInterval: "five minutes",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.retryInterval")))
		})

		It("should reject more than 50 Feeds at apply time", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-too-many-feeds",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feeds := make([]rss2discordv1alpha1.FeedSpec, 51)
			for i := range feeds {
				feeds[i] = rss2discordv1alpha1.FeedSpec{RSSUrl: fmt.Sprintf("https://example.com/feed-%d.xml", i)}
			}

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-too-many-feeds",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-too-many-feeds"},
						Key:                  secretURLKey,
					},
					Feeds: feeds,
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.feeds")))
		})
	})

	Describe("Discord rate limiting", func() {
		It("should requeue using Discord's Retry-After duration without persisting the unsent entry's validators", func() {
			const feedGroupName = "test-feedgroup-rate-limited"

			By("Creating a mock Discord webhook server that rate-limits the first delivery")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			discordServer.FailNextRequestsRateLimited(1, "2")

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Rate Limited Article",
					description: "Should survive a 429",
					link:        "https://example.com/rate-limited-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "rate-limited-article",
				},
			))
			rssServer.SetETag(`"v1"`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-rate-limited",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup resource")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-rate-limited"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running reconciliation, where Discord rate-limits the send")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the reconcile requeues after Discord's Retry-After duration, not the normal interval")
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			By("Verifying the new ETag was NOT persisted, since the entry was never sent")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).To(ContainSubstring("rate limited"))

			By("Reconciling again, where the send succeeds and the previously unsent entry is delivered")
			result, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(result.RequeueAfter).To(Equal(30 * time.Minute))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(Equal(`"v1"`))
			Expect(feedsWithError(updated)).To(Equal(0))
		})

		It("should stop sending the rest of a feed's entries after the first 429", func() {
			const feedGroupName = "test-feedgroup-rate-limited-stop"

			By("Creating a mock Discord server that rate-limits many deliveries")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			discordServer.FailNextRequestsRateLimited(10, "2")

			By("Creating a mock RSS feed with several catch-up entries")
			type item = struct {
				title       string
				description string
				link        string
				pubDate     string
				guid        string
			}
			entries := make([]item, 0, 5)
			for i := range 5 {
				entries = append(entries, item{
					title:       fmt.Sprintf("Article %d", i),
					description: "body",
					link:        fmt.Sprintf("https://example.com/article%d", i),
					pubDate:     time.Now().Add(-time.Duration(5-i) * time.Hour).Format(time.RFC1123Z),
					guid:        fmt.Sprintf("guid-%d", i),
				})
			}
			rssServer := NewMockRSSServer(createRSSFeed(entries...))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "discord-webhook-rl-stop", Namespace: namespace},
				Data:       map[string][]byte{secretURLKey: []byte(discordServer.URL())},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating FeedGroup with a catch-up limit of 5")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: feedGroupName, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-rl-stop"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					CatchUpLimit:  5,
					Feeds:         []rss2discordv1alpha1.FeedSpec{{RSSUrl: rssServer.URL()}},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running reconciliation")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only ONE POST was attempted before backing off, not one per entry")
			Expect(discordServer.DeliveryAttempts()).To(Equal(1))
			Expect(discordServer.MessageCount()).To(Equal(0))
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))
		})

		It("should stop processing the rest of a FeedGroup's feeds once one feed's send is rate-limited", func() {
			const feedGroupName = "test-feedgroup-rate-limited-multi-feed"

			By("Creating a mock Discord server that rate-limits the very first delivery")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()
			discordServer.FailNextRequestsRateLimited(1, "3")

			By("Creating two mock RSS feed servers, each with one entry")
			firstFeed := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "First Feed Article",
					description: "Triggers the rate limit",
					link:        "https://example.com/first-feed-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "first-feed-article",
				},
			))
			defer firstFeed.Close()
			secondFeed := NewMockRSSServer(createRSSFeed(
				struct {
					title       string
					description string
					link        string
					pubDate     string
					guid        string
				}{
					title:       "Second Feed Article",
					description: "Should not be reached this reconcile",
					link:        "https://example.com/second-feed-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "second-feed-article",
				},
			))
			defer secondFeed.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "discord-webhook-rl-multi-feed", Namespace: namespace},
				Data:       map[string][]byte{secretURLKey: []byte(discordServer.URL())},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating a FeedGroup with both feeds, first feed listed first")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: feedGroupName, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-rl-multi-feed"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: firstFeed.URL()},
						{RSSUrl: secondFeed.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running reconciliation")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the second feed was fetched concurrently but never processed/sent, since the shared webhook was already rate-limited")
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the reconcile requeues after the rate limit's Retry-After duration")
			Expect(result.RequeueAfter).To(Equal(3 * time.Second))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, firstFeed.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, secondFeed.URL()).LastChecked).To(BeEmpty())
		})
	})

	Describe("FeedGroup validation", func() {
		It("should reject a FeedGroup with duplicate feed URLs", func() {
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-dup-url", Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "any-secret"},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
						{RSSUrl: exampleFeedURL},
					},
				},
			}
			err := k8sClient.Create(ctx, feedGroup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rssUrl must be unique"))
		})

		It("should accept a FeedGroup with distinct feed URLs", func() {
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-distinct-url", Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "any-secret"},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: "https://example.com/a.xml"},
						{RSSUrl: "https://example.com/b.xml"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())
		})
	})

	Describe("Concurrent feed fetching", func() {
		It("should fetch and send for every feed when the FeedGroup exceeds the concurrent-fetch limit", func() {
			const feedGroupName = "test-feedgroup-concurrent"
			const numFeeds = 15 // exceeds maxConcurrentFetches (10), to exercise the semaphore-bounded fan-out under -race.

			By("Creating one mock RSS server per feed, each with a single unique entry")
			rssServers := make([]*MockRSSServer, numFeeds)
			feeds := make([]rss2discordv1alpha1.FeedSpec, numFeeds)
			for i := range rssServers {
				rssServers[i] = NewMockRSSServer(createRSSFeed(
					struct {
						title       string
						description string
						link        string
						pubDate     string
						guid        string
					}{
						title:       fmt.Sprintf("Concurrent Article %d", i),
						description: "fetched concurrently",
						link:        fmt.Sprintf("https://example.com/concurrent-%d", i),
						pubDate:     time.Now().Format(time.RFC1123Z),
						guid:        fmt.Sprintf("concurrent-%d", i),
					},
				))
				feeds[i] = rss2discordv1alpha1.FeedSpec{RSSUrl: rssServers[i].URL()}
			}
			defer func() {
				for _, s := range rssServers {
					s.Close()
				}
			}()

			By("Creating a mock Discord webhook server shared by all feeds")
			discordServer := NewMockDiscordServer()
			defer discordServer.Close()

			By("Creating a secret with Discord webhook URL")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-concurrent",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					secretURLKey: []byte(discordServer.URL()),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("Creating a FeedGroup with more feeds than the concurrent-fetch limit")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-concurrent"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "5m",
					Feeds:         feeds,
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
			}

			By("Running reconciliation")
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Minute))

			By("Verifying every feed was fetched and its entry delivered, despite exceeding the concurrency limit")
			Expect(discordServer.MessageCount()).To(Equal(numFeeds))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedsWithError(updated)).To(Equal(0))
			for _, s := range rssServers {
				Expect(feedStatusFor(updated, s.URL()).LastChecked).NotTo(BeEmpty())
				Expect(feedStatusFor(updated, s.URL()).LastSeenEntry).NotTo(BeEmpty())
			}
		})
	})

	Describe("Resource not found", func() {
		It("should handle non-existent FeedGroup gracefully", func() {
			By("Running reconciliation for non-existent resource")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			defer deleteFeedGroupMetrics(namespace, "nonexistent")

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("Verifying no reconcile-duration series was created for a FeedGroup that doesn't exist")
			count, err := reconcileDurationSampleCountErr(namespace, "nonexistent")
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(uint64(0)))
		})
	})

	Describe("Username admission validation", func() {
		// minimalFeedGroup returns a FeedGroup that satisfies every field
		// other than Username, so these tests isolate the CEL rule on
		// Username rather than tripping over unrelated required fields.
		minimalFeedGroup := func(name, username string) *rss2discordv1alpha1.FeedGroup {
			return &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Username: username,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
					},
				},
			}
		}

		DescribeTable("should reject usernames Discord itself would reject",
			func(username string) {
				err := k8sClient.Create(ctx, minimalFeedGroup("username-rejected", username))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("username must not contain"))
			},
			Entry("contains clyde", "ClydeBot"),
			Entry("contains discord", "totally-discord-official"),
		)

		It("should reject a username over 80 characters", func() {
			err := k8sClient.Create(ctx, minimalFeedGroup("username-too-long", strings.Repeat("a", 81)))
			Expect(err).To(HaveOccurred())
		})

		It("should accept a username that doesn't violate either constraint", func() {
			feedGroup := minimalFeedGroup("username-ok", "tech-news-bot")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())
			Expect(k8sClient.Delete(ctx, feedGroup)).To(Succeed())
		})
	})

	Describe("SetupWithManager", func() {
		It("should default the event recorder when none is set", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:  k8sClient.Scheme(),
				Metrics: metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())

			reconciler := &FeedGroupReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(reconciler.Recorder).To(BeNil())

			Expect(reconciler.SetupWithManager(mgr)).To(Succeed())
			Expect(reconciler.Recorder).NotTo(BeNil())
		})
	})
})
