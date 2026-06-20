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
	server      *httptest.Server
	feedContent string
	statusCode  int
}

// NewMockRSSServer creates a new mock RSS server with test feed data
func NewMockRSSServer(feedContent string) *MockRSSServer {
	m := &MockRSSServer{
		feedContent: feedContent,
		statusCode:  http.StatusOK,
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(m.statusCode)
			_, _ = w.Write([]byte(m.feedContent))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))

	return m
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
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			d.messagesReceived++
			d.bodiesReceived = append(d.bodiesReceived, string(body))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))

	discord.AllowedWebhookHosts[d.server.Listener.Addr().(*net.TCPAddr).IP.String()] = true

	return d
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
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
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
