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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const defaultMessageFormat = "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"

// maxLastSentPerFeed bounds how many sent-entry hashes are retained per feed
// in Status.LastSent. Without a cap this map grows by one key per sent
// message forever, bloating the FeedGroup status subresource indefinitely.
const maxLastSentPerFeed = 200

// FeedGroupReconciler reconciles a FeedGroup object
type FeedGroupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RSSClient            *rss.Client
	DiscordClientBuilder func(webhookURL string) *discord.Client
}

// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *FeedGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("feedgroup", req.NamespacedName)
	ctx = logf.IntoContext(ctx, log)

	var feedGroup v1alpha1.FeedGroup
	if err := r.Get(ctx, req.NamespacedName, &feedGroup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get FeedGroup")
		return ctrl.Result{}, err
	}

	r.setDefaultStatusMaps(&feedGroup)

	webhookURL, err := r.resolveWebhookURL(ctx, &feedGroup)
	if err != nil {
		log.Error(err, "failed to resolve Discord webhook URL")
		feedGroup.Status.LastError["webhook"] = err.Error()
		return r.requeueWithStatus(ctx, &feedGroup, feedGroup.Spec.RetryInterval, 5*time.Minute)
	}

	if webhookURL == "" {
		err = fmt.Errorf("discord webhook URL is empty")
		log.Error(err, "invalid webhook secret")
		feedGroup.Status.LastError["webhook"] = err.Error()
		return r.requeueWithStatus(ctx, &feedGroup, feedGroup.Spec.RetryInterval, 5*time.Minute)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rssClient := r.RSSClient
	if rssClient == nil {
		rssClient = rss.NewClient(nil)
	}
	discordBuilder := r.DiscordClientBuilder
	if discordBuilder == nil {
		discordBuilder = discord.NewClient
	}
	discordClient := discordBuilder(webhookURL)

	activeFeeds := make([]v1alpha1.FeedSpec, 0, len(feedGroup.Spec.Feeds))
	for _, feed := range feedGroup.Spec.Feeds {
		if feed.RSSUrl == "" || feed.Paused {
			continue
		}
		activeFeeds = append(activeFeeds, feed)
	}

	// Feeds are independent network fetches, so fetch them concurrently
	// rather than paying each feed's fetch timeout sequentially. Sending and
	// status-map updates below remain single-threaded, so no locking is
	// needed for the rest of the reconcile.
	fetched := make([][]rss.Entry, len(activeFeeds))
	fetchErrs := make([]error, len(activeFeeds))
	var wg sync.WaitGroup
	for i, feed := range activeFeeds {
		wg.Add(1)
		go func(i int, rssURL string) {
			defer wg.Done()
			fetched[i], fetchErrs[i] = rssClient.FetchEntries(ctx, rssURL)
		}(i, feed.RSSUrl)
	}
	wg.Wait()

	wantRetry := false
	var discordRetryAfter time.Duration
	for i, feed := range activeFeeds {
		retry, rateLimitRetryAfter := r.processFeed(ctx, &feedGroup, feed, fetched[i], fetchErrs[i], discordClient, now)
		if retry {
			wantRetry = true
		}
		if rateLimitRetryAfter > discordRetryAfter {
			discordRetryAfter = rateLimitRetryAfter
		}
	}

	if discordRetryAfter > 0 {
		return r.requeueWithStatus(ctx, &feedGroup, "", discordRetryAfter)
	}

	interval := feedGroup.Spec.Interval
	fallback := 30 * time.Minute
	if wantRetry {
		interval = feedGroup.Spec.RetryInterval
		fallback = 5 * time.Minute
	}

	return r.requeueWithStatus(ctx, &feedGroup, interval, fallback)
}

// processFeed fetches/filters/sends entries for a single feed and updates
// feedGroup's status maps accordingly. It returns whether the reconcile
// should retry sooner than the normal interval, and, if Discord rate
// limited the webhook, how long to wait before the next attempt.
func (r *FeedGroupReconciler) processFeed(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	feed v1alpha1.FeedSpec,
	entries []rss.Entry,
	fetchErr error,
	discordClient *discord.Client,
	now string,
) (wantRetry bool, rateLimitRetryAfter time.Duration) {
	log := logf.FromContext(ctx)

	feedGroup.Status.LastChecked[feed.RSSUrl] = now
	if _, ok := feedGroup.Status.LastSent[feed.RSSUrl]; !ok {
		feedGroup.Status.LastSent[feed.RSSUrl] = map[string]string{}
	}

	if fetchErr != nil {
		log.Error(fetchErr, "failed to fetch RSS feed", "url", feed.RSSUrl)
		feedGroup.Status.LastError[feed.RSSUrl] = fetchErr.Error()
		feedGroup.Status.RetryCount[feed.RSSUrl]++
		return feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries), 0
	}

	if len(entries) == 0 {
		delete(feedGroup.Status.LastError, feed.RSSUrl)
		feedGroup.Status.RetryCount[feed.RSSUrl] = 0
		return false, 0
	}

	filterRegex, err := compileFilterRegex(feed.Filter)
	if err != nil {
		log.Error(err, "invalid filter regex", "url", feed.RSSUrl)
		feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
		return false, 0
	}

	tmpl, err := compileMessageTemplate(feedGroup, &feed)
	if err != nil {
		log.Error(err, "invalid Discord message template", "url", feed.RSSUrl)
		feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
		return true, 0
	}

	slices.SortStableFunc(entries, func(a, b rss.Entry) int {
		return a.Published.Compare(b.Published)
	})

	lastSeenID := feedGroup.Status.LastSeenEntry[feed.RSSUrl]
	hasSeenID := lastSeenID != ""
	foundLastSeen := !hasSeenID

	for _, entry := range entries {
		if hasSeenID && !foundLastSeen {
			if entry.ID == lastSeenID {
				foundLastSeen = true
			}
			continue
		}

		entryKey := computeEntryKey(entry)
		if _, alreadySent := feedGroup.Status.LastSent[feed.RSSUrl][entryKey]; alreadySent {
			continue
		}

		if !matchesFilter(feed.Filter, entry, filterRegex) {
			continue
		}

		message, err := renderMessage(tmpl, entry)
		if err != nil {
			log.Error(err, "failed to render Discord message", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
			wantRetry = true
			continue
		}

		if err := discordClient.SendMessage(ctx, message); err != nil {
			log.Error(err, "failed to send Discord message", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()

			var rateLimitErr *discord.RateLimitError
			if errors.As(err, &rateLimitErr) {
				wantRetry = true
				if rateLimitErr.RetryAfter > rateLimitRetryAfter {
					rateLimitRetryAfter = rateLimitErr.RetryAfter
				}
			} else {
				feedGroup.Status.RetryCount[feed.RSSUrl]++
				if feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries) {
					wantRetry = true
				}
			}
			continue
		}

		feedGroup.Status.LastSent[feed.RSSUrl][entryKey] = now
		feedGroup.Status.LastSeenEntry[feed.RSSUrl] = entry.ID
		delete(feedGroup.Status.LastError, feed.RSSUrl)
		feedGroup.Status.RetryCount[feed.RSSUrl] = 0
	}

	pruneLastSent(feedGroup.Status.LastSent[feed.RSSUrl], maxLastSentPerFeed)

	return wantRetry, rateLimitRetryAfter
}

func (r *FeedGroupReconciler) resolveWebhookURL(ctx context.Context, feedGroup *v1alpha1.FeedGroup) (string, error) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      feedGroup.Spec.DiscordWebhookSecretRef.Name,
		Namespace: feedGroup.Namespace,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		return "", err
	}

	value, ok := secret.Data[feedGroup.Spec.DiscordWebhookSecretRef.Key]
	if !ok {
		return "", fmt.Errorf("secret %s missing key %q", secretKey.Name, feedGroup.Spec.DiscordWebhookSecretRef.Key)
	}

	return strings.TrimSpace(string(value)), nil
}

func (r *FeedGroupReconciler) setDefaultStatusMaps(feedGroup *v1alpha1.FeedGroup) {
	if feedGroup.Status.LastChecked == nil {
		feedGroup.Status.LastChecked = map[string]string{}
	}
	if feedGroup.Status.LastSeenEntry == nil {
		feedGroup.Status.LastSeenEntry = map[string]string{}
	}
	if feedGroup.Status.LastSent == nil {
		feedGroup.Status.LastSent = map[string]map[string]string{}
	}
	if feedGroup.Status.LastError == nil {
		feedGroup.Status.LastError = map[string]string{}
	}
	if feedGroup.Status.RetryCount == nil {
		feedGroup.Status.RetryCount = map[string]int{}
	}
}

func (r *FeedGroupReconciler) requeueWithStatus(ctx context.Context, feedGroup *v1alpha1.FeedGroup, interval string, fallback time.Duration) (ctrl.Result, error) {
	duration, err := parseDurationWithDefault(interval, fallback)
	if err != nil {
		return ctrl.Result{}, err
	}

	feedGroup.Status.ObservedGeneration = feedGroup.Generation
	setReadyCondition(feedGroup)

	if err := r.Status().Update(ctx, feedGroup); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: duration}, nil
}

// setReadyCondition summarizes the outcome of a reconciliation into a single
// "Ready" condition, leaving the existing per-feed status maps (LastError,
// RetryCount, etc.) as the detailed source of truth.
func setReadyCondition(feedGroup *v1alpha1.FeedGroup) {
	status := metav1.ConditionTrue
	reason := "Reconciled"
	message := "All feeds processed successfully"

	if len(feedGroup.Status.LastError) > 0 {
		status = metav1.ConditionFalse
		reason = "FeedErrors"
		message = fmt.Sprintf("%d feed(s) reporting errors", len(feedGroup.Status.LastError))
	}

	apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: feedGroup.Generation,
	})
}

func computeEntryKey(entry rss.Entry) string {
	hash := sha256.Sum256([]byte(entry.ID + "|" + entry.Link + "|" + entry.Title))
	return hex.EncodeToString(hash[:])
}

// compileFilterRegex compiles a feed's filter regex once per feed per
// reconcile, instead of recompiling it for every entry in the feed.
func compileFilterRegex(filter *v1alpha1.Filter) (*regexp.Regexp, error) {
	if filter == nil || strings.TrimSpace(filter.Regex) == "" {
		return nil, nil
	}

	re, err := regexp.Compile(filter.Regex)
	if err != nil {
		return nil, fmt.Errorf("invalid filter regex %q: %w", filter.Regex, err)
	}
	return re, nil
}

func matchesFilter(filter *v1alpha1.Filter, entry rss.Entry, filterRegex *regexp.Regexp) bool {
	if filter == nil {
		return true
	}

	if filterRegex != nil && !filterRegex.MatchString(entry.Title+"\n"+entry.Description) {
		return false
	}

	if len(filter.Keywords) == 0 {
		return true
	}

	content := strings.ToLower(strings.TrimSpace(entry.Title + "\n" + entry.Description))
	for _, keyword := range filter.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		if strings.Contains(content, strings.ToLower(keyword)) {
			return true
		}
	}

	return false
}

// compileMessageTemplate parses a feed's Discord message template once per
// feed per reconcile, instead of reparsing it for every entry sent.
func compileMessageTemplate(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) (*template.Template, error) {
	tmplText := strings.TrimSpace(feed.Format)
	if tmplText == "" {
		tmplText = strings.TrimSpace(feedGroup.Spec.Format)
	}
	if tmplText == "" {
		tmplText = defaultMessageFormat
	}

	return template.New("discordMessage").Parse(tmplText)
}

func renderMessage(tmpl *template.Template, entry rss.Entry) (string, error) {
	data := struct {
		Title       string
		Description string
		Link        string
		Published   string
	}{
		Title:       entry.Title,
		Description: entry.Description,
		Link:        entry.Link,
		Published:   entry.Published.UTC().Format(time.RFC3339),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// pruneLastSent caps a feed's sent-entry dedup map at max entries, dropping
// the oldest (by recorded RFC3339 send timestamp, which sorts lexically)
// first. Without this, LastSent grows by one key per sent message forever.
func pruneLastSent(sent map[string]string, max int) {
	if len(sent) <= max {
		return
	}

	keys := make([]string, 0, len(sent))
	for key := range sent {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return strings.Compare(sent[a], sent[b])
	})

	for _, key := range keys[:len(keys)-max] {
		delete(sent, key)
	}
}

func parseDurationWithDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback, fmt.Errorf("invalid duration %q: %w", value, err)
	}

	return duration, nil
}

func maxRetryCount(specRetries int) int {
	if specRetries < 1 {
		return 1
	}
	return specRetries
}

// SetupWithManager sets up the controller with the Manager.
func (r *FeedGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.FeedGroup{}).
		Named("feedgroup").
		Complete(r)
}
