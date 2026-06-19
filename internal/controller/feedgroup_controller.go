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
	"fmt"
	"regexp"
	"slices"
	"strings"
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

	wantRetry := false
	for _, feed := range feedGroup.Spec.Feeds {
		if feed.RSSUrl == "" || feed.Paused {
			continue
		}

		feedGroup.Status.LastChecked[feed.RSSUrl] = now
		if _, ok := feedGroup.Status.LastSent[feed.RSSUrl]; !ok {
			feedGroup.Status.LastSent[feed.RSSUrl] = map[string]string{}
		}

		entries, err := rssClient.FetchEntries(ctx, feed.RSSUrl)
		if err != nil {
			log.Error(err, "failed to fetch RSS feed", "url", feed.RSSUrl)
			feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
			feedGroup.Status.RetryCount[feed.RSSUrl]++
			if feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries) {
				wantRetry = true
			}
			continue
		}

		if len(entries) == 0 {
			delete(feedGroup.Status.LastError, feed.RSSUrl)
			feedGroup.Status.RetryCount[feed.RSSUrl] = 0
			continue
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

			matched, err := matchesFilter(feed.Filter, entry)
			if err != nil {
				log.Error(err, "invalid filter regex", "url", feed.RSSUrl)
				feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
				continue
			}
			if !matched {
				continue
			}

			message, err := renderMessage(&feedGroup, &feed, entry)
			if err != nil {
				log.Error(err, "failed to render Discord message", "url", feed.RSSUrl)
				feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
				wantRetry = true
				continue
			}

			if err := discordClient.SendMessage(ctx, message); err != nil {
				log.Error(err, "failed to send Discord message", "url", feed.RSSUrl)
				feedGroup.Status.LastError[feed.RSSUrl] = err.Error()
				feedGroup.Status.RetryCount[feed.RSSUrl]++
				if feedGroup.Status.RetryCount[feed.RSSUrl] < maxRetryCount(feedGroup.Spec.Retries) {
					wantRetry = true
				}
				continue
			}

			feedGroup.Status.LastSent[feed.RSSUrl][entryKey] = now
			feedGroup.Status.LastSeenEntry[feed.RSSUrl] = entry.ID
			delete(feedGroup.Status.LastError, feed.RSSUrl)
			feedGroup.Status.RetryCount[feed.RSSUrl] = 0
		}
	}

	interval := feedGroup.Spec.Interval
	fallback := 30 * time.Minute
	if wantRetry {
		interval = feedGroup.Spec.RetryInterval
		fallback = 5 * time.Minute
	}

	return r.requeueWithStatus(ctx, &feedGroup, interval, fallback)
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

func matchesFilter(filter *v1alpha1.Filter, entry rss.Entry) (bool, error) {
	if filter == nil {
		return true, nil
	}

	content := strings.ToLower(strings.TrimSpace(entry.Title + "\n" + entry.Description))

	if strings.TrimSpace(filter.Regex) != "" {
		matched, err := regexp.MatchString(filter.Regex, entry.Title+"\n"+entry.Description)
		if err != nil {
			return false, fmt.Errorf("invalid filter regex %q: %w", filter.Regex, err)
		}
		if !matched {
			return false, nil
		}
	}

	if len(filter.Keywords) == 0 {
		return true, nil
	}

	for _, keyword := range filter.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		if strings.Contains(content, strings.ToLower(keyword)) {
			return true, nil
		}
	}

	return false, nil
}

func renderMessage(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec, entry rss.Entry) (string, error) {
	tmplText := strings.TrimSpace(feed.Format)
	if tmplText == "" {
		tmplText = strings.TrimSpace(feedGroup.Spec.Format)
	}
	if tmplText == "" {
		tmplText = defaultMessageFormat
	}

	tmpl, err := template.New("discordMessage").Parse(tmplText)
	if err != nil {
		return "", err
	}

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
