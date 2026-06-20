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

// +kubebuilder:object:generate=true
// +groupName=rss2discord.maverickd650.dev
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Filter defines filtering rules for RSS feed entries.
type Filter struct {
	// Regex is a regular expression to match against entry title/description.
	// +optional
	Regex string `json:"regex,omitempty"`

	// Keywords is a list of keywords to match (OR logic).
	// +optional
	Keywords []string `json:"keywords,omitempty"`
}

// FeedSpec defines the configuration for a single RSS feed.
type FeedSpec struct {
	// RSSUrl is the URL of the RSS feed to fetch. Only http:// and https:// are supported.
	// +kubebuilder:validation:Pattern=`^https?://`
	RSSUrl string `json:"rssUrl"`

	// Filter defines how to filter entries from this feed.
	// +optional
	Filter *Filter `json:"filter,omitempty"`

	// Format is the template for Discord messages for this feed.
	// Overrides the group-level format if set.
	// +optional
	Format string `json:"format,omitempty"`

	// Paused stops processing this feed if set to true.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// FeedGroupSpec defines the desired state of a FeedGroup.
type FeedGroupSpec struct {
	// DiscordWebhookSecretRef points to the Secret containing the Discord webhook URL.
	// The secret must contain the webhook URL under the specified key.
	DiscordWebhookSecretRef corev1.SecretKeySelector `json:"discordWebhookSecretRef"`

	// Interval is the duration between feed checks (e.g., "30m", "1h").
	// +kubebuilder:default="30m"
	// +optional
	Interval string `json:"interval,omitempty"`

	// Format is the default template for Discord messages.
	// Supports placeholders: {{.Title}}, {{.Description}}, {{.Link}}, {{.Published}}.
	// +kubebuilder:default="**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"
	// +optional
	Format string `json:"format,omitempty"`

	// Retries is the number of times to retry failed operations (fetch/send).
	// +kubebuilder:default=3
	// +optional
	Retries int `json:"retries,omitempty"`

	// RetryInterval is the duration between retries (e.g., "5m").
	// +kubebuilder:default="5m"
	// +optional
	RetryInterval string `json:"retryInterval,omitempty"`

	// Feeds is the list of RSS feed configurations in this group.
	Feeds []FeedSpec `json:"feeds"`

	// CatchUpLimit caps how many backlog entries are sent to Discord the
	// first time a feed is reconciled (i.e. before it has a recorded
	// LastSeenEntry). Without this, adding a feed with a long history
	// floods the webhook with every existing entry at once. Entries beyond
	// the limit are treated as already seen and are not sent.
	// +kubebuilder:default=5
	// +optional
	CatchUpLimit int `json:"catchUpLimit,omitempty"`
}

// FeedGroupStatus defines the observed state of a FeedGroup.
type FeedGroupStatus struct {
	// LastChecked is a map of feed URL to the last time it was checked (RFC3339 timestamp).
	// +optional
	LastChecked map[string]string `json:"lastChecked,omitempty"`

	// LastSeenEntry is a map of feed URL to the last seen entry identifier (GUID or Link).
	// Used to fetch only new entries after restarts.
	// +optional
	LastSeenEntry map[string]string `json:"lastSeenEntry,omitempty"`

	// LastSent is a map of feed URL to a map of entry hash to timestamp (RFC3339).
	// Tracks which entries have been sent to Discord to avoid duplicates.
	// +optional
	LastSent map[string]map[string]string `json:"lastSent,omitempty"`

	// LastError is a map of feed URL to the last error encountered.
	// +optional
	LastError map[string]string `json:"lastError,omitempty"`

	// RetryCount is a map of feed URL to the number of consecutive retries.
	// +optional
	RetryCount map[string]int `json:"retryCount,omitempty"`

	// ObservedGeneration is the most recent generation observed for this FeedGroup
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the FeedGroup's
	// overall state, following the standard Kubernetes condition conventions.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// ConditionTypeReady indicates whether the FeedGroup's most recent
// reconciliation completed without errors across all of its feeds.
const ConditionTypeReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=feedgroups,scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// FeedGroup is the Schema for the feedgroups API.
type FeedGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FeedGroupSpec   `json:"spec,omitempty"`
	Status FeedGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FeedGroupList contains a list of FeedGroup.
type FeedGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedGroup `json:"items"`
}
