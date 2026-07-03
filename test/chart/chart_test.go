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

// Package chart holds golden-file regression tests for dist/chart. They
// don't validate template syntax (helm lint already does that in CI) --
// they pin down the *content* of the hand-tuned files mise run
// helm-chart-refresh knows to preserve (see CLAUDE.md), so a refresh
// regression shows up as a test failure instead of only in `git diff`
// during code review.
package chart

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// update regenerates every golden file from the current chart output
// instead of comparing against it. Run `go test ./test/chart -update` after
// an intentional chart change (or after `mise run helm-chart-refresh`) and
// review the diff -- that review is the actual point of this flag.
var update = flag.Bool("update", false, "update golden files instead of comparing against them")

const (
	chartReleaseName = "chart-golden-test"
	chartNamespace   = "chart-golden-test-ns"
)

// helmChartLabelPattern matches the helm.sh/chart label's value, which
// embeds Chart.yaml's version (e.g. "rss2discord-operator-0.10.1"). That
// version bumps on every release-please release, so it's normalized before
// golden comparison rather than pinned literally -- otherwise every release
// PR would spuriously fail this test.
var helmChartLabelPattern = regexp.MustCompile(`helm\.sh/chart: \S+`)

// normalize strips volatile content from rendered chart output before it's
// compared against (or written as) a golden file.
func normalize(rendered string) string {
	return helmChartLabelPattern.ReplaceAllString(rendered, "helm.sh/chart: SCRUBBED")
}

// renderChartTemplate runs `helm template`, scoped with -s to a single
// template file, against a fixed release name/namespace. setValues are
// "key=value" pairs each passed as a separate --set flag. manager.image.tag
// is always pinned to a fixed value: its default otherwise falls back to
// Chart.yaml's AppVersion, which is just as volatile across releases as the
// helm.sh/chart label. It fails (never skips) when helm is missing -- a
// skippable regression test isn't one, and `mise run test` (which this runs
// under) provides helm via mise.
func renderChartTemplate(t *testing.T, showOnly string, setValues ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH -- this test requires it (run via `mise run test`, "+
			"which installs the mise-pinned helm): %v", err)
	}

	chartDir := filepath.Join("..", "..", "dist", "chart")
	args := make([]string, 0, 7+2*(len(setValues)+1))
	args = append(args,
		"template", chartReleaseName, chartDir,
		"--namespace", chartNamespace,
		"-s", showOnly,
	)
	for _, v := range append([]string{"manager.image.tag=golden-test"}, setValues...) {
		args = append(args, "--set", v)
	}

	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template -s %s failed: %v\n%s", showOnly, err, out)
	}
	return normalize(string(out))
}

// TestChartGolden renders each of the chart's hand-tuned files (see
// CLAUDE.md's dist/chart section) under the value combinations that flip
// their content, and compares the result byte-for-byte against a checked-in
// golden file. helm lint (the only other chart check in CI) validates
// syntax, not content, so this is the only thing that would catch e.g.
// mise run helm-chart-refresh silently dropping the ServiceMonitor's
// native-histogram block.
func TestChartGolden(t *testing.T) {
	cases := []struct {
		name      string
		showOnly  string
		setValues []string
	}{
		// manager.yaml under its defaults, and with metrics.secure=false
		// (flips the --metrics-secure=false arg onto the container).
		{
			name:     "manager_defaults",
			showOnly: "templates/manager/manager.yaml",
		},
		{
			name:      "manager_metrics_insecure",
			showOnly:  "templates/manager/manager.yaml",
			setValues: []string{"metrics.secure=false"},
		},
		// controller-manager-metrics-service.yaml: metrics.secure flips the
		// port name between "https" and "http".
		{
			name:     "metrics_service_defaults",
			showOnly: "templates/metrics/controller-manager-metrics-service.yaml",
		},
		{
			name:      "metrics_service_insecure",
			showOnly:  "templates/metrics/controller-manager-metrics-service.yaml",
			setValues: []string{"metrics.secure=false"},
		},
		// controller-manager-metrics-monitor.yaml (the ServiceMonitor):
		// scrapeNativeHistograms toggles whether the
		// scrapeProtocols/scrapeClassicHistograms/scrapeNativeHistograms
		// block appears at all.
		{
			name:      "metrics_monitor_native_histograms_true",
			showOnly:  "templates/prometheus/controller-manager-metrics-monitor.yaml",
			setValues: []string{"prometheus.enabled=true", "prometheus.scrapeNativeHistograms=true"},
		},
		{
			name:      "metrics_monitor_native_histograms_false",
			showOnly:  "templates/prometheus/controller-manager-metrics-monitor.yaml",
			setValues: []string{"prometheus.enabled=true", "prometheus.scrapeNativeHistograms=false"},
		},
		// prometheus-rule.yaml: wholly custom, no kubebuilder equivalent.
		{
			name:      "prometheus_rule_enabled",
			showOnly:  "templates/prometheus/prometheus-rule.yaml",
			setValues: []string{"prometheusRule.enabled=true"},
		},
		// grafana-dashboard.yaml: emits both the ConfigMap (embedding
		// dashboards/feedgroup-overview.json) and the GrafanaDashboard CR.
		{
			name:      "grafana_dashboard_enabled",
			showOnly:  "templates/prometheus/grafana-dashboard.yaml",
			setValues: []string{"grafanaDashboard.enabled=true"},
		},
		// allow-metrics-traffic.yaml (NetworkPolicy): simple, but flagged as
		// a permutation to cover in the T7 task spec.
		{
			name:      "network_policy_enabled",
			showOnly:  "templates/network-policy/allow-metrics-traffic.yaml",
			setValues: []string{"networkPolicy.enabled=true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderChartTemplate(t, tc.showOnly, tc.setValues...)
			goldenPath := filepath.Join("testdata", tc.name+".golden.yaml")

			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("failed to write golden file %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("failed to read golden file %s (run `go test ./test/chart -update` to create it): %v",
					goldenPath, err)
			}
			if got != string(want) {
				t.Errorf("rendered output for %s does not match %s.\n"+
					"If this is an intentional chart change, run `go test ./test/chart -update` "+
					"and review the golden diff.\n--- got ---\n%s\n--- want ---\n%s",
					tc.showOnly, goldenPath, got, string(want))
			}
		})
	}
}
