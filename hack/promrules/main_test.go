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

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunExtractsSpec(t *testing.T) {
	input := `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: example
spec:
  groups:
    - name: example.rules
      rules:
        - alert: Example
          expr: up == 0
`
	var out bytes.Buffer
	if err := run(strings.NewReader(input), &out); err != nil {
		t.Fatalf("run() returned an error: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "apiVersion") || strings.Contains(got, "kind:") {
		t.Errorf("output still contains the CRD wrapper, want only .spec's content:\n%s", got)
	}
	if !strings.Contains(got, "groups:") || !strings.Contains(got, "alert: Example") {
		t.Errorf("output missing expected rule content:\n%s", got)
	}
}

func TestRunRejectsMissingSpec(t *testing.T) {
	input := `apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-rule
`
	var out bytes.Buffer
	if err := run(strings.NewReader(input), &out); err == nil {
		t.Fatal("run() succeeded on input with no 'spec' key, want an error")
	}
}

func TestRunRejectsInvalidYAML(t *testing.T) {
	var out bytes.Buffer
	if err := run(strings.NewReader("not: valid: yaml: at: all:"), &out); err == nil {
		t.Fatal("run() succeeded on invalid YAML, want an error")
	}
}
