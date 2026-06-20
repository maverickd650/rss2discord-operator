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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rss2discordv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const (
	secretURLKey           = "url"
	defaultInterval        = "30m"
	feedGroupNameBasic     = "test-feedgroup"
	feedGroupNameNoSecret  = "test-feedgroup-no-secret"
	feedGroupNameRetry     = "test-feedgroup-retry"
	feedGroupNameFilter    = "test-feedgroup-filter"
	feedGroupNameKeywords  = "test-feedgroup-keywords"
	feedGroupNamePaused    = "test-feedgroup-paused"
	feedGroupNameTimestamp = "test-feedgroup-timestamp"
)

// testRSSClient is a plain (non-SSRF-guarded) RSS client used in tests so
// that reconciliation can reach mock servers bound to loopback addresses.
// Production code goes through rss.NewClient(nil), which guards against
// connecting to non-public addresses.
func testRSSClient() *rss.Client {
	return rss.NewClient(&http.Client{})
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
	bodiesReceived   []string
	failNext         int
	failStatus       int
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

		if d.failNext > 0 {
			d.failNext--
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
					Name:      "discord-webhook",
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
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook"},
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

			Expect(updated.Status.LastChecked).To(HaveKey(rssServer.URL()))
			Expect(updated.Status.LastSeenEntry).To(HaveKey(rssServer.URL()))

			By("Verifying no errors in status")
			Expect(updated.Status.LastError).To(BeEmpty())

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
			Expect(afterFirst.Status.FeedETag).To(HaveKeyWithValue(rssServer.URL(), `"v1"`))
			lastSeenAfterFirst := afterFirst.Status.LastSeenEntry[rssServer.URL()]
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
			Expect(afterSecond.Status.LastSeenEntry[rssServer.URL()]).To(Equal(lastSeenAfterFirst))
			Expect(afterSecond.Status.LastError).To(BeEmpty())
			Expect(afterSecond.Status.RetryCount[rssServer.URL()]).To(Equal(0))

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
			Expect(afterFirst.Status.FeedETag).NotTo(HaveKey(rssServer.URL()))
			Expect(afterFirst.Status.LastSeenEntry).NotTo(HaveKey(rssServer.URL()))
			Expect(afterFirst.Status.LastError).To(HaveKey(rssServer.URL()))

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
			Expect(afterSecond.Status.FeedETag).To(HaveKeyWithValue(rssServer.URL(), `"v1"`))
			Expect(afterSecond.Status.LastSeenEntry[rssServer.URL()]).NotTo(BeEmpty())
			Expect(afterSecond.Status.LastError).To(BeEmpty())
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
			Expect(updated.Status.LastError).To(HaveKey(rssServer.URL))

			By("Verifying the Ready condition reflects the failure")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("FeedErrors"))
			Expect(updated.Status.RetryCount[rssServer.URL]).To(Equal(1))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Status.RetryCount[rssServer.URL]).To(Equal(2))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("FetchFailed")))
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
			Expect(updated.Status.LastError).To(HaveKey(rssServer.URL()))
			Expect(updated.Status.RetryCount[rssServer.URL()]).To(Equal(1))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted, instead of retrying forever")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Status.RetryCount[rssServer.URL()]).To(Equal(2))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("RenderFailed")))
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
			Expect(updated.Status.LastSeenEntry).To(HaveKey(rssServer.URL()))
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
			Expect(updated.Status.LastSeenEntry).To(HaveKey(rssServer.URL()))
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
			Expect(updated.Status.LastSeenEntry).NotTo(HaveKey(rssServer.URL()))
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

			Expect(updated.Status.LastChecked).To(HaveKey(rssServer.URL()))
			// Should be a valid RFC3339 timestamp
			_, err = time.Parse(time.RFC3339, updated.Status.LastChecked[rssServer.URL()])
			Expect(err).NotTo(HaveOccurred())
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
			Expect(afterFirst.Status.LastChecked).To(HaveKey(rssServer.URL()))
			Expect(afterFirst.Status.FeedETag).To(HaveKey(rssServer.URL()))

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
			Expect(afterSecond.Status.LastChecked).NotTo(HaveKey(rssServer.URL()))
			Expect(afterSecond.Status.LastSeenEntry).NotTo(HaveKey(rssServer.URL()))
			Expect(afterSecond.Status.LastSent).NotTo(HaveKey(rssServer.URL()))
			Expect(afterSecond.Status.FeedETag).NotTo(HaveKey(rssServer.URL()))
			Expect(afterSecond.Status.FeedLastModified).NotTo(HaveKey(rssServer.URL()))
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
						{RSSUrl: "https://example.com/feed.xml"},
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
						{RSSUrl: "https://example.com/feed.xml"},
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

	Describe("Resource not found", func() {
		It("should handle non-existent FeedGroup gracefully", func() {
			By("Running reconciliation for non-existent resource")
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})
	})
})
