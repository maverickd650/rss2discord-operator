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
	"html"
	"regexp"
	"slices"
	"strconv"
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
	"k8s.io/client-go/tools/record"
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

// defaultCatchUpLimit is used when FeedGroupSpec.CatchUpLimit is unset
// (zero), which happens both for an explicit 0 and for CRDs applied before
// the field existed (the kubebuilder default only takes effect through the
// API server's defaulting webhook/CRD schema).
const defaultCatchUpLimit = 5

// maxDiscordMessageLength is Discord's hard cap on webhook message content;
// the API rejects anything longer. Full article bodies in a feed's
// description can easily exceed this once stripped to plain text.
const maxDiscordMessageLength = 2000

// FeedGroupReconciler reconciles a FeedGroup object
type FeedGroupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RSSClient            *rss.Client
	DiscordClientBuilder func(webhookURL string) *discord.Client
	Recorder             record.EventRecorder
}

// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	pruneRemovedFeedStatus(&feedGroup)

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
	// needed for the rest of the reconcile. Reading (not writing)
	// feedGroup.Status here is safe concurrently with itself, since nothing
	// mutates it until after wg.Wait().
	fetched := make([]rss.FetchResult, len(activeFeeds))
	fetchErrs := make([]error, len(activeFeeds))
	var wg sync.WaitGroup
	for i, feed := range activeFeeds {
		wg.Add(1)
		go func(i int, rssURL string) {
			defer wg.Done()
			validators := rss.CacheValidators{
				ETag:         feedGroup.Status.FeedETag[rssURL],
				LastModified: feedGroup.Status.FeedLastModified[rssURL],
			}
			fetched[i], fetchErrs[i] = rssClient.FetchEntries(ctx, rssURL, validators)
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
	fetchResult rss.FetchResult,
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
		feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, outcomeFetchError).Inc()

		retry := feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries)
		if !retry {
			r.recordPersistentFailure(feedGroup, feed.RSSUrl, "FetchFailed", fetchErr)
		}
		return retry, 0
	}

	// Persisting the new validators is deferred until the end of this
	// function (and skipped entirely if anything below needs a retry): if a
	// send fails, storing the new ETag now would make the next reconcile's
	// conditional GET return 304 before the unsent entry is ever retried,
	// silently dropping it.
	persistValidators := func() {
		if fetchResult.ETag != "" {
			feedGroup.Status.FeedETag[feed.RSSUrl] = fetchResult.ETag
		}
		if fetchResult.LastModified != "" {
			feedGroup.Status.FeedLastModified[feed.RSSUrl] = fetchResult.LastModified
		}
	}

	if fetchResult.NotModified {
		// A 304 has nothing new to retry, so it's always safe to persist.
		persistValidators()
		delete(feedGroup.Status.LastError, feed.RSSUrl)
		feedGroup.Status.RetryCount[feed.RSSUrl] = 0
		return false, 0
	}

	entries := fetchResult.Entries
	if len(entries) == 0 {
		// Nothing to send, so it's always safe to persist.
		persistValidators()
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

	embedSpec := resolveEmbedSpec(feedGroup, &feed)

	var contentTmpl, descriptionTmpl, threadNameTmpl *template.Template
	if embedSpec != nil && embedSpec.Enabled {
		descriptionTmpl, err = compileTemplate("discordEmbedDescription", embedSpec.DescriptionFormat, defaultDescriptionFormat)
	} else {
		contentTmpl, err = compileMessageTemplate(feedGroup, &feed)
	}
	if err != nil {
		log.Error(err, "invalid Discord message template", "url", feed.RSSUrl)
		feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
		return true, 0
	}

	if strings.TrimSpace(feed.ForumThreadName) != "" {
		threadNameTmpl, err = compileTemplate("discordThreadName", feed.ForumThreadName, "")
		if err != nil {
			log.Error(err, "invalid Discord forum thread name template", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
			return true, 0
		}
	}

	slices.SortStableFunc(entries, func(a, b rss.Entry) int {
		return a.Published.Compare(b.Published)
	})

	lastSeenID := feedGroup.Status.LastSeenEntry[feed.RSSUrl]
	hasSeenID := lastSeenID != ""
	foundLastSeen := !hasSeenID

	if !hasSeenID {
		entries = limitCatchUp(entries, feedGroup.Spec.CatchUpLimit)
	}

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

		discordMessage, err := buildDiscordMessage(feedGroup, embedSpec, contentTmpl, descriptionTmpl, threadNameTmpl, &feed, entry)
		if err != nil {
			log.Error(err, "failed to render Discord message", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
			wantRetry = true
			continue
		}

		if err := discordClient.SendMessage(ctx, discordMessage); err != nil {
			log.Error(err, "failed to send Discord message", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()

			var rateLimitErr *discord.RateLimitError
			if errors.As(err, &rateLimitErr) {
				wantRetry = true
				feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, outcomeRateLimited).Inc()
				if rateLimitErr.RetryAfter > rateLimitRetryAfter {
					rateLimitRetryAfter = rateLimitErr.RetryAfter
				}
			} else {
				feedGroup.Status.RetryCount[feed.RSSUrl]++
				feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, outcomeSendError).Inc()
				if feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries) {
					wantRetry = true
				} else {
					r.recordPersistentFailure(feedGroup, feed.RSSUrl, "SendFailed", err)
				}
			}
			continue
		}

		feedGroup.Status.LastSent[feed.RSSUrl][entryKey] = now
		feedGroup.Status.LastSeenEntry[feed.RSSUrl] = entry.ID
		delete(feedGroup.Status.LastError, feed.RSSUrl)
		feedGroup.Status.RetryCount[feed.RSSUrl] = 0
		feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, outcomeSent).Inc()
	}

	pruneLastSent(feedGroup.Status.LastSent[feed.RSSUrl], maxLastSentPerFeed)

	// Only persist now that every entry from this fetch was either sent or
	// filtered out; if anything is still pending retry, keep the old
	// validators so the next fetch returns the full body again instead of a
	// 304 that would skip the unsent entry for good.
	if !wantRetry && rateLimitRetryAfter == 0 {
		persistValidators()
	}

	return wantRetry, rateLimitRetryAfter
}

// recordPersistentFailure emits a Warning Event on feedGroup once a feed's
// fetch or send retries are exhausted, so the failure is visible via
// `kubectl describe feedgroup` instead of requiring a controller log dive.
// It's a no-op if Recorder is unset (e.g. in unit tests that construct the
// reconciler directly rather than through SetupWithManager).
func (r *FeedGroupReconciler) recordPersistentFailure(feedGroup *v1alpha1.FeedGroup, url, reason string, err error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(feedGroup, corev1.EventTypeWarning, reason,
		"feed %s: giving up after exhausting retries: %v", url, err)
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
	if feedGroup.Status.FeedETag == nil {
		feedGroup.Status.FeedETag = map[string]string{}
	}
	if feedGroup.Status.FeedLastModified == nil {
		feedGroup.Status.FeedLastModified = map[string]string{}
	}
}

// pruneRemovedFeedStatus deletes every per-feed-URL status entry that no
// longer corresponds to a feed in feedGroup.Spec.Feeds. Without this, editing
// or removing a feed leaves its LastChecked/LastSeenEntry/LastSent/LastError/
// RetryCount/FeedETag/FeedLastModified entries in status forever, the same
// unbounded-growth problem maxLastSentPerFeed guards against one level down.
// Paused feeds keep their status, since they're still part of the spec.
func pruneRemovedFeedStatus(feedGroup *v1alpha1.FeedGroup) {
	specURLs := make(map[string]bool, len(feedGroup.Spec.Feeds))
	for _, feed := range feedGroup.Spec.Feeds {
		specURLs[feed.RSSUrl] = true
	}

	for url := range feedGroup.Status.LastChecked {
		if !specURLs[url] {
			delete(feedGroup.Status.LastChecked, url)
		}
	}
	for url := range feedGroup.Status.LastSeenEntry {
		if !specURLs[url] {
			delete(feedGroup.Status.LastSeenEntry, url)
		}
	}
	for url := range feedGroup.Status.LastSent {
		if !specURLs[url] {
			delete(feedGroup.Status.LastSent, url)
		}
	}
	for url := range feedGroup.Status.LastError {
		if !specURLs[url] {
			delete(feedGroup.Status.LastError, url)
		}
	}
	for url := range feedGroup.Status.RetryCount {
		if !specURLs[url] {
			delete(feedGroup.Status.RetryCount, url)
		}
	}
	for url := range feedGroup.Status.FeedETag {
		if !specURLs[url] {
			delete(feedGroup.Status.FeedETag, url)
		}
	}
	for url := range feedGroup.Status.FeedLastModified {
		if !specURLs[url] {
			delete(feedGroup.Status.FeedLastModified, url)
		}
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

	return compileTemplate("discordMessage", tmplText, defaultMessageFormat)
}

// compileTemplate parses text as a Discord message template, falling back
// to fallback when text is blank.
func compileTemplate(name, text, fallback string) (*template.Template, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = fallback
	}
	return template.New(name).Parse(text)
}

// resolveEmbedSpec returns the feed's embed config if it set one, otherwise
// falling back to the FeedGroup's. Returns nil if neither configured one,
// meaning the feed sends plain-text content.
func resolveEmbedSpec(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) *v1alpha1.EmbedSpec {
	if feed.Embed != nil {
		return feed.Embed
	}
	return feedGroup.Spec.Embed
}

// parseHexColor parses a "#RRGGBB" or "RRGGBB" string into Discord's 24-bit
// embed color integer, defaulting to 0 (black/unset) for blank or malformed
// input rather than failing the whole message.
func parseHexColor(s string) int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 16, 32)
	if err != nil {
		return 0
	}
	return int(v)
}

// htmlBlockTagRegex matches HTML tags that delimit block-level content (or
// line breaks), which are converted to newlines so stripped text retains
// paragraph/list structure instead of running together.
var htmlBlockTagRegex = regexp.MustCompile(`(?i)</?\s*(p|li|br|div|h[1-6])\b[^>]*>`)

// htmlTagRegex matches any remaining HTML tag, stripped entirely.
var htmlTagRegex = regexp.MustCompile(`<[^>]+>`)

// blankLineRegex collapses runs of 3+ newlines (left behind once tags are
// stripped) down to a single blank line.
var blankLineRegex = regexp.MustCompile(`\n{3,}`)

// stripHTML converts an RSS/Atom entry's description into Discord-friendly
// plain text. Many feeds (e.g. the Guardian's) ship descriptions as raw
// HTML, which Discord otherwise renders as literal tag soup.
func stripHTML(input string) string {
	text := htmlBlockTagRegex.ReplaceAllString(input, "\n")
	text = htmlTagRegex.ReplaceAllString(text, "")
	text = html.UnescapeString(text)

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	text = blankLineRegex.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")

	return strings.TrimSpace(text)
}

// defaultDescriptionFormat is used for an embed's description when neither
// the feed nor the group set EmbedSpec.DescriptionFormat.
const defaultDescriptionFormat = "{{.Description}}"

// Discord's embed and forum API limits; messages exceeding these are
// rejected outright rather than truncated server-side.
const (
	maxEmbedTitleLength       = 256
	maxEmbedDescriptionLength = 4096
	maxForumThreadNameLength  = 100
)

func renderTemplate(tmpl *template.Template, entry rss.Entry, max int) (string, error) {
	data := struct {
		Title       string
		Description string
		Link        string
		Published   string
	}{
		Title:       entry.Title,
		Description: stripHTML(entry.Description),
		Link:        entry.Link,
		Published:   entry.Published.UTC().Format(time.RFC3339),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}

	return truncateMessage(buf.String(), max), nil
}

// buildDiscordMessage renders entry into the message that should actually
// be sent: an embed (colored bubble with title/description/thumbnail) when
// embedSpec is enabled, or plain text content otherwise. It also resolves
// forum-channel thread targeting (ForumThreadID takes precedence over a
// rendered ForumThreadName) and group-level webhook branding.
func buildDiscordMessage(
	feedGroup *v1alpha1.FeedGroup,
	embedSpec *v1alpha1.EmbedSpec,
	contentTmpl, descriptionTmpl, threadNameTmpl *template.Template,
	feed *v1alpha1.FeedSpec,
	entry rss.Entry,
) (discord.Message, error) {
	msg := discord.Message{
		Username:  feedGroup.Spec.Username,
		AvatarURL: feedGroup.Spec.AvatarURL,
	}

	if embedSpec != nil && embedSpec.Enabled {
		description, err := renderTemplate(descriptionTmpl, entry, maxEmbedDescriptionLength)
		if err != nil {
			return discord.Message{}, err
		}
		msg.Embed = &discord.Embed{
			Title:        truncateMessage(entry.Title, maxEmbedTitleLength),
			Description:  description,
			URL:          entry.Link,
			Color:        parseHexColor(embedSpec.Color),
			Timestamp:    entry.Published.UTC().Format(time.RFC3339),
			ThumbnailURL: entry.Image,
			AuthorName:   embedSpec.AuthorName,
			FooterText:   embedSpec.FooterText,
		}
	} else {
		content, err := renderTemplate(contentTmpl, entry, maxDiscordMessageLength)
		if err != nil {
			return discord.Message{}, err
		}
		msg.Content = content
	}

	if strings.TrimSpace(feed.ForumThreadID) != "" {
		msg.ThreadID = feed.ForumThreadID
	} else if threadNameTmpl != nil {
		threadName, err := renderTemplate(threadNameTmpl, entry, maxForumThreadNameLength)
		if err != nil {
			return discord.Message{}, err
		}
		msg.ThreadName = threadName
	}

	return msg, nil
}

// truncateMessage trims content to at most max characters (by rune), since
// Discord rejects webhook messages over its content length limit outright
// rather than truncating them itself.
func truncateMessage(content string, max int) string {
	runes := []rune(content)
	if len(runes) <= max {
		return content
	}

	const ellipsis = "…"
	return string(runes[:max-len([]rune(ellipsis))]) + ellipsis
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

// limitCatchUp trims entries to at most limit, keeping the most recent ones
// (entries is assumed sorted ascending by Published). A non-positive limit
// falls back to defaultCatchUpLimit rather than disabling the cap, so a feed
// can never dump its entire backlog on first reconcile.
func limitCatchUp(entries []rss.Entry, limit int) []rss.Entry {
	if limit <= 0 {
		limit = defaultCatchUpLimit
	}
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
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
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("feedgroup-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.FeedGroup{}).
		Named("feedgroup").
		Complete(r)
}
