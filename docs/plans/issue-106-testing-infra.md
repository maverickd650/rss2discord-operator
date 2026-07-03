# Execution plan: issue 106 — testing infrastructure

Status: ready for pickup · Written: 2026-07-03 · Verified against `main` @ `cf23976`
Tracking issue: #106 (fuzzing depth, FeedGroup e2e, observability contract tests, CI efficiency)

This document breaks issue 106 into 13 self-contained tasks (T1–T13). **Each task is sized
for one PR and can be picked up by an independent agent with no context beyond this file,
`CLAUDE.md`, and `AGENTS.md`.** Pick a task, check the [conflict matrix](#conflict-matrix)
for anything in flight, follow the spec, and check the box on issue 106 when merged.

---

## Ground rules (apply to every task — read before starting any of them)

1. **Never weaken the SSRF guards.** `internal/rss/client.go` (`newDefaultHTTPClient`,
   `isPublicIP`) and `internal/discord/client.go` (`AllowedWebhookHosts`, HTTPS-only) must not
   change semantics, and no "test-only" escape hatches go into production code. Tests that
   need local `httptest` servers use the existing helpers in `internal/controller/*_test.go`
   (`testRSSClient()`, registering into `discord.AllowedWebhookHosts`). Never fuzz or test
   through `Client.SendMessage` / `Client.FetchEntries` — fuzz the pure helpers.
2. **Local == CI: everything CI runs must be a `mise run <task>`.** New tools are pinned in
   `.mise/config.toml` `[tools]`; every `[tools]` change must regenerate `mise.lock`
   (CI installs with `--locked` and fails otherwise). Never invoke `go test`/lint directly
   in workflows.
3. **New required-check job names must be duplicated in
   `.github/workflows/skip-docs-checks.yml`** (it fakes `Lint` / `Test` / `E2E Tests` on
   markdown-only PRs). This plan deliberately adds **zero** new PR-triggered job names —
   new CI work is either a step inside the existing `Test` job or a cron/`workflow_dispatch`
   workflow (which can never be a required check). Keep it that way.
4. **No bare cross-repo references** (`owner/repo#123`) in anything that lands in commits,
   PR titles/bodies, or comments — use full URLs. Same-repo `#106` references are fine.
   See CLAUDE.md for why.
5. **Out of scope everywhere:** coverage for `cmd/`, `api/`, `test/` (deliberately excluded
   in `codecov.yml`); test-framework migration (Ginkgo for envtest + stdlib elsewhere stays).
6. Workflow hygiene when touching `.github/workflows/`: SHA-pinned actions with `# vX.Y.Z`
   comments, top-level `permissions: {}` with per-job least privilege,
   `persist-credentials: false` on checkout, a `concurrency` group. Copy the style of
   `test.yml`.
7. PRs use Conventional Commits and the checklist in `.github/PULL_REQUEST_TEMPLATE.md`.
   After any `*.go` change: `mise run lint-fix` and `mise run test`.

## Corrections to the issue text (verified 2026-07-03)

The issue was written against `main` @ `2988873`; ~10 commits have landed since. These
corrections are load-bearing — task specs below already incorporate them:

- **There is no `internal/discord/payload.go` sanitization pipeline.** Reality:
  `@everyone`/`@here` suppression is the `discordAllowedMentions{Parse: []string{}}` struct
  literal always set on payloads in `internal/discord/client.go`; embed length clamping is
  `clampEmbedTotalLength` / `truncateRunes` / `embedToPayload` (same file,
  `maxEmbedTotalLength = 6000`); the `javascript:`/`data:` URI stripping is
  `httpURLOrEmpty` in `internal/controller/feedgroup_controller.go` (http/https allowlist),
  and the plain-message clamp is `truncateMessage` there too (`maxDiscordMessageLength = 2000`).
  The "second fuzz target" therefore becomes **two** targets in two packages (T2).
- **Chart value keys are `*.enabled`, not `*.enable`**: `prometheus.enabled`,
  `prometheusRule.enabled`, `grafanaDashboard.enabled`, `networkPolicy.enabled`, plus
  `prometheus.scrapeNativeHistograms`.
- **`config/samples/rss2discord_v1alpha1_feedgroup.yaml` is broken today**: its
  `apiVersion` reads `rss2discord.rss2discord.maverickd650.dev/v1alpha1` (doubled prefix);
  the real group is `rss2discord.maverickd650.dev`. `kubectl apply` of the sample fails on a
  real cluster. T3 fixes this — and the fact that nothing caught it is itself the argument
  for the failure-path e2e.
- **`testing/synctest` is already in use** (`internal/discord/ratelimiter_test.go`, landed in
  PR #111), so T10 scopes to the controller's wall-clock backoff logic only.
- PR #115 added optional OTel tracing on the outbound RSS/Discord HTTP paths — rebase
  carefully if a task touches `internal/rss/client.go` / `internal/discord/client.go`
  test files.

## Task index

| ID  | Title | Issue item | Priority | Wave | Status |
|-----|-------|-----------|----------|------|--------|
| T1  | `mise run fuzz` + scheduled fuzz workflow with cumulative corpus | 1a | High | 1 | |
| T2  | Discord + controller sanitization fuzz targets (+ benchmarks) | 1b, 6d | High | 2 | |
| T3  | Failure-path FeedGroup e2e + fix sample apiVersion | 2a | High | 1 | |
| T4  | Pin the Kind node image | 2b (part) | Medium | 2 | ✅ done |
| T5  | Observability contract test (outcomes ↔ dashboard ↔ alerts) | 3a | High | 1 | ✅ done |
| T6  | promtool check/test rules in CI | 3b | Medium | 2 | |
| T7  | Chart golden-file snapshot tests | 4a, 4b | Medium | 1 | |
| T8  | Cache envtest binaries in CI | 5a | Medium | 1 | ✅ done |
| T9  | Codecov patch status enforcing | 5b | Low — **land last** | 3 | |
| T10 | synctest / clock-extraction spike | 6a | Experimental | 2 | |
| T11 | goleak TestMain for `internal/rss` + `internal/discord` | 6b | Experimental | 1 | |
| T12 | Mutation-testing experiment (go-gremlins) | 6c | Experimental, gated | 3 | |
| T13 | ClusterFuzzLite evaluation | 1c | **Deferred** | — | |

**Waves** (tasks within a wave are parallel-safe; see conflict matrix for exceptions):
Wave 1: T1, T3, T5, T7, T8, T11 → Wave 2: T2, T4, T6, T10 → Wave 3: T12, then T9 last.
Deferred: T13 and the e2e k8s-version matrix (optional half of T4).

## Dependency notes

- T1 → T2 is a *soft* dependency: T1's fuzz task auto-discovers `Fuzz*` targets, so either
  order works, but landing T1 first means T2's targets get scheduled fuzzing for free.
- T8 → T6: both edit `.github/workflows/test.yml`; land T8 (smaller) first.
- T6 vs T12: both add `[tools]` entries and regenerate `mise.lock`; serialize them.
- T9 lands last so every code-adding PR above merges under the current informational
  Codecov regime.
- T13 is gated on T1 having ≥4 scheduled runs of history.

## Conflict matrix

| File | Tasks touching it | Resolution |
|---|---|---|
| `.mise/config.toml` | T1, T4, T6, T12 | Additive edits in different sections; trivial rebases. Serialize T6/T12 (both also touch `mise.lock`). |
| `mise.lock` | T6, T12 | Serialize. Regenerate via mise; never hand-edit. |
| `.github/workflows/test.yml` | T6, T8 | Serialize: T8 first. |
| `test/e2e/e2e_test.go` | T3 only | — |
| `internal/discord/` test files | T2 (`fuzz_test.go`), T11 (`main_test.go`) | Different files. Note T11's goleak TestMain will also wrap T2's seed runs — that's fine and desirable. |
| `internal/controller/` test files | T2, T5, T10 | Different files (`fuzz_test.go` / `observability_contract_test.go` / T10's helper tests). T10 also refactors `feedgroup_controller.go` — rebase T2/T5 after it if concurrent. |
| `go.mod` / `go.sum` | T11 | The `go mod tidy` gate in `test.yml` fails the PR if these aren't committed. |
| `skip-docs-checks.yml` | none, by design | Only the deferred e2e matrix would touch it. |

---

## T1 — `mise run fuzz` task + scheduled fuzz workflow with cumulative corpus

**Why:** `FuzzParseFeed` only ever runs its seed corpus in CI (`mise run test` runs fuzz
targets as plain tests). Nothing runs with `-fuzz`, so the fuzzer never explores. Feed XML
is the most attacker-controlled input in the system; a parser panic takes down the shared
reconcile loop.

**Files:** `.mise/config.toml` (new `[tasks.fuzz]`), `.github/workflows/fuzz.yml` (new).

**Steps:**
1. Add `[tasks.fuzz]`. Shape: enumerate packages with fuzz targets
   (`go test -list '^Fuzz' <pkg>` per package under `./internal/...`), then for each target run
   `go test -run '^$' -fuzz "^<target>\$" -fuzztime "${FUZZTIME:-5m}" <pkg>`.
   Two hard requirements:
   - `-fuzz` accepts exactly **one** target per invocation — loop, don't glob.
   - Always pass `-run '^$'` — otherwise, once T2 lands a fuzz target in
     `internal/controller`, the package's envtest Ginkgo suite boots as a prerequisite and
     fails without `KUBEBUILDER_ASSETS`.
2. Add `.github/workflows/fuzz.yml`:
   - `on: { schedule: [{ cron: "43 4 * * 1" }], workflow_dispatch: {} }` — minute offset
     from the existing weekly crons (govulncheck `13 4`, codeql `27 4`, scorecard `37 4`).
     **No PR trigger**, so no required-check / `skip-docs-checks.yml` implications
     (document that in a header comment).
   - Job (`contents: read`): checkout → `./.github/actions/setup-go-env` →
     `actions/cache` (same SHA pin as in `setup-go-env/action.yml`) restore+save of the
     cumulative corpus at `$(go env GOCACHE)/fuzz` — cache keys are immutable, so use
     `key: ${{ runner.os }}-fuzz-corpus-${{ github.run_id }}` with
     `restore-keys: ${{ runner.os }}-fuzz-corpus-` and save `if: always()` → `mise run fuzz`
     → `if: failure()` upload-artifact of `**/testdata/fuzz/**` (Go writes crashing inputs
     to `<pkg>/testdata/fuzz/<Target>/` in the checkout).
3. If a crasher is ever found: reproduce locally, fix, and commit the input as a permanent
   regression seed under `testdata/fuzz/` in a follow-up PR.

**Acceptance criteria:**
- `FUZZTIME=10s mise run fuzz` runs `FuzzParseFeed` (and any future targets) locally.
- Workflow has no PR trigger and follows the repo's pinning/permissions conventions.

**Verification:** `FUZZTIME=10s mise run fuzz` · `mise run test` · `mise run lint` ·
after merge, `workflow_dispatch` the workflow once and confirm the corpus cache is saved.

**Constraints reminder:** ground rules 1–3 apply; the fuzz task must not touch guard code.

## T2 — Discord + controller sanitization fuzz targets, plus `b.Loop` benchmarks

**Why:** feed titles/descriptions/links flow straight into the Discord payload pipeline —
the same adversarial input as the XML parser. Issue item 1b, corrected for where the code
actually lives (see corrections above). Bundles issue item 6d (benchmarks) since it's the
same files.

**Files:** `internal/discord/fuzz_test.go` (new), `internal/controller/fuzz_test.go` (new),
`internal/rss/` benchmark (may live in existing `parse_test.go` or a new `bench_test.go`).

**Steps:**
1. `FuzzEmbedPayload` (package `discord`): fuzz embed field strings
   (title/description/author/footer/URL) through `embedToPayload` +
   `clampEmbedTotalLength`, then `json.Marshal` the payload struct. Invariants:
   - no panic;
   - total embed length ≤ `maxEmbedTotalLength` (6000) — reuse `EmbedTotalLengthOverflow`;
   - `truncateRunes` output is ≤ max runes and `utf8.ValidString`;
   - marshaled JSON always contains `"allowed_mentions"` with an **empty** `parse` array
     (the `@everyone`/`@here` suppression invariant — the field must never be omitted or nil).
2. `FuzzControllerSanitizers` (package `controller`, plain Go test — no envtest): fuzz
   - `httpURLOrEmpty`: output is either `""` or parses with scheme `http`/`https` —
     `javascript:`/`data:`/anything else never survives;
   - `truncateMessage`: output ≤ `maxDiscordMessageLength` (2000), valid UTF-8, no panic,
     and the returned overflow count is consistent with the truncation.
3. Seed both targets with adversarial inputs: mixed-width runes, combining characters,
   `@everyone`, `@here`, `javascript:alert(1)`, `data:text/html,...`, zero bytes,
   4-byte emoji straddling the clamp boundary.
4. Benchmarks using `for b.Loop()`: `BenchmarkParseFeed` (`internal/rss`, input sized near
   the 10 MiB `maxFeedResponseBytes` cap — guard against accidental quadratic behavior) and
   `BenchmarkEmbedToPayload` (`internal/discord`).

**Do not** fuzz through `Client.SendMessage` or `Client.FetchEntries` (network + SSRF
guards) — pure helpers only. No production code changes.

**Acceptance criteria:** seeds pass under `mise run test`; each target survives
`FUZZTIME=30s mise run fuzz`; PR body notes the issue-text correction (no `payload.go`).

**Verification:** `mise run test` · `FUZZTIME=30s mise run fuzz` ·
`go test -bench=. -run '^$' ./internal/rss ./internal/discord`.

## T3 — Failure-path FeedGroup e2e + fix sample apiVersion

**Why:** `test/e2e/e2e_test.go` deliberately skips reconciliation (mock webhooks would need
weakening `AllowedWebhookHosts`), but a *failure-path* spec needs no mock at all, and it
covers exactly what envtest can't: CRD schema on a real API server, controller RBAC on
`/status`, Events, and the metrics endpoint. Bonus: the sample CR is broken today and only
a real-cluster apply catches it.

**Files:** `config/samples/rss2discord_v1alpha1_feedgroup.yaml`,
`test/e2e/e2e_test.go` (new `It` in the existing `Ordered` Describe, after the metrics spec
so it reuses the ClusterRoleBinding/token/curl plumbing).

**Key mechanics (verified in code, rely on these):**
- An unresolvable host (`.invalid` TLD is RFC 6761-guaranteed NXDOMAIN) produces a
  `*net.DNSError` → `classifyNetworkError` → `classDNSFailure`, which is **non-permanent**:
  `RetryCount` increments each attempt, the feed retries on `spec.retryInterval`, and the
  Warning Event fires exactly once when `RetryCount == maxRetryCount(spec.retries)`.
  (A guard-blocked private IP classifies as `Other` and is less specific — use DNS.)
- The Event (events.k8s.io `Eventf`) has **reason `FetchFailed`**, action
  `RetriesExhausted`, note `feed <url>: giving up after exhausting retries: ...` —
  assert on reason + note substring, not on the action.
- `resolveWebhookURL` runs **before** any fetch, so the CR's
  `discordWebhookSecretRef` must point at a real Secret with a non-empty value, e.g.
  `https://discord.com/api/webhooks/000/dummy`. Nothing is ever sent (fetch fails first),
  so this doesn't touch the webhook guard.
- Metric to assert: `rss2discord_feed_operations_total{...,outcome="fetch_error_dns_failure"}`.
- `rssUrl` must match `^https?://`; spec defaults: `retries` 3, `retryInterval` "5m",
  `interval` "30m" — override all three for test speed.

**Steps:**
1. Fix the sample's `apiVersion` to `rss2discord.maverickd650.dev/v1alpha1`
   (samples are explicitly editable; nothing regenerates them).
2. New spec, in its own namespace: create the dummy webhook Secret; apply a CR derived from
   the fixed sample with `feeds[0].rssUrl: https://feeds.invalid/feed.xml`, `retries: 2`,
   `retryInterval: 5s`, `interval: 1m`. Assert, in order:
   a. the apply succeeds (CRD schema + validation on a real API server);
   b. `Eventually`: `status.feeds[0].conditions` has `Reachable=False` with reason
      `DNSFailure` (exercises controller RBAC on `/status` + `classify.go` end-to-end);
   c. `Eventually`: a Warning Event with reason `FetchFailed` and note containing
      `giving up after exhausting retries` exists (fires once — do not write a
      "fires again" `Consistently` assertion, it can't succeed);
   d. re-run the token-authenticated curl-pod metrics fetch (new pod name, e.g.
      `curl-metrics-feedgroup`; extend `AfterAll` cleanup) and assert the body contains
      `outcome="fetch_error_dns_failure"` with the CR's namespace/name labels.
   Keep `Eventually` windows ≥ 60s: requeue jitter (`jitterDuration`) applies on top of
   `retryInterval`.
3. Update the "deliberately not exercising FeedGroup" comment block near the bottom of
   `e2e_test.go`: the failure path is now covered; the success path still isn't, and why.
4. *Optional, flag in the PR if taken:* a second feed `https://10.0.0.1/feed.xml`
   asserting `Reachable=False` with reason `Other` and a message containing the guard's
   refusal text — proves the SSRF guard operates in-cluster. Read-only observation of the
   guard; the guard itself must not change.

**Acceptance criteria:** `mise run test-e2e` green including the new spec; sample applies
cleanly on the Kind cluster; zero production-code changes.

**Verification:** `mise run test-e2e` ·
`kubectl apply --dry-run=server -f config/samples/` against the Kind cluster.

## T4 — Pin the Kind node image ✅ done

**Why:** `[tasks.test-e2e]` runs `kind create cluster` with no `--image`, so the k8s version
under e2e test silently changes whenever the kind binary is bumped.

**Files:** `.mise/config.toml` only.

**Steps:**
1. Add `KIND_NODE_IMAGE` to `[env]`, defaulting to a **digest-pinned**
   `kindest/node:v1.3x.y@sha256:...` taken from the kind v0.32.0 release notes
   (https://github.com/kubernetes-sigs/kind/releases — cite as full URL only), matching the
   repo's k8s library minor (see `go list -m k8s.io/api`, currently 1.36).
2. Pass `--image "$KIND_NODE_IMAGE"` in `[tasks.test-e2e]`'s `kind create cluster`.
3. Comment the pin: it must be bumped together with the `kind` tool pin, and Renovate
   cannot see it (it lives in a task script) — manual bump.

**Deferred (do not take without maintainer sign-off):** the k8s version matrix in
`test-e2e.yml`. A matrix renames the required check from `E2E Tests` to `E2E Tests (...)`,
which requires branch-protection changes **and** matching duplicate job names in
`skip-docs-checks.yml`.

**Acceptance criteria:** `mise run test-e2e` green; `kind get clusters` empty afterwards
(the cleanup `trap` still fires).

**Verification:** `mise run test-e2e`.

## T5 — Observability contract test ✅ done

**Why:** CLAUDE.md documents a strict contract — every outcome label emitted by
`internal/controller` must be matched by the anchored regexes in the Grafana dashboard
(`dist/chart/dashboards/feedgroup-overview.json`) and the PrometheusRule
(`dist/chart/templates/prometheus/prometheus-rule.yaml`). Nothing enforces it; a new
outcome silently falls out of dashboards and alerts.

**Files:** `internal/controller/observability_contract_test.go` (new; same package so it
can reference the unexported constants — zero production diff).

**The contract, verified today:**
- Emitted outcomes: `sent`, `render_error`, `rate_limited`, plus `fetch_error_<reason>` /
  `send_error_<reason>` for every `failureClass.metricReason` (`not_found`, `gone`,
  `rate_limited`, `server_error`, `client_error`, `timeout`, `dns_failure`, `parse_error`,
  `unrecognized_format`, `webhook_invalid`, `other`). Build them in-test via
  `fetchErrorOutcome`/`sendErrorOutcome` over a test-file `allFailureClasses` slice
  referencing the `class*` vars in `classify.go`.
- Exhaustiveness guard so a new `failureClass` can't be forgotten: read `classify.go`
  source in the test (`os.ReadFile` — test cwd is the package dir) and assert the count of
  `failureClass{` composite literals equals `len(allFailureClasses)`.
- Dashboard matchers to extract: `outcome=~"fetch_error_.+|send_error_.+|render_error"`
  (several panels), `outcome="sent"`, `outcome!~"rate_limited"`.
- Alert selectors: `outcome=~"fetch_error_.+"` + `label_replace(..., "fetch_error_(.+)")`,
  `outcome="rate_limited"`, `outcome=~"send_error_.+"` + `label_replace(..., "send_error_(.+)")`.
- **Documented asymmetries — encode as explicit exceptions, not failures:** `render_error`
  is dashboard-only by design (no alert); `sent` is a success (no alert); the send-ratio
  panel's negative matcher `outcome!~"rate_limited"` intentionally matches almost
  everything.

**Steps:**
1. Render the chart from the test:
   `helm template contract-test ../../dist/chart --set prometheus.enabled=true --set prometheusRule.enabled=true --set grafanaDashboard.enabled=true`
   (helm is on PATH under mise, and the test runs inside `mise run test`, so local == CI
   holds by construction). **Fail — never skip — if helm is missing**, with a message
   pointing at `mise run test`; a skippable contract isn't a contract.
2. Extract every `outcome=~"..."` / `outcome="..."` / `outcome!~"..."` matcher from the
   rendered dashboard ConfigMap JSON and the PrometheusRule exprs. A pragmatic string/regex
   scan is fine — document that it's not a PromQL parser.
3. Cross-check both directions, anchoring regexes as `^(?:...)$` (Prometheus label regexes
   are fully anchored; Go's regexp is the same RE2 syntax):
   - every expected outcome matches ≥1 dashboard matcher;
   - every `fetch_error_*` / `send_error_*` / `rate_limited` outcome matches ≥1 alert
     selector (exceptions above);
   - every extracted positive regex matches ≥1 expected outcome (no dead regexes);
   - the metric name in every extracted expr is `rss2discord_feed_operations_total`.

**Acceptance criteria:** passes under `mise run test`; deleting an outcome regex from the
dashboard JSON locally makes it fail; adding a dummy `failureClass{...}` literal to
`classify.go` makes the exhaustiveness guard fail.

**Verification:** `mise run test` ·
`go test ./internal/controller/ -run TestObservabilityContract -v` (after
`mise run manifests generate`).

**Implementation notes (deviations from the steps above, both intentional):**
- Renders each template individually via `helm template -s <file> --set <its .enabled>=true`
  (two invocations: the dashboard ConfigMap, the PrometheusRule) rather than one combined
  render with all three flags set — simpler to attribute a matcher to its source document,
  and `prometheus.enabled` (the ServiceMonitor) doesn't affect either file so it's omitted.
- The coverage cross-checks (both directions) test whether a matcher's pattern *names* an
  outcome (`outcomeMatcher.namesOutcome`, anchored-regex-or-literal-equality against the
  pattern text) rather than simulating true Prometheus series-selection semantics. This
  matters for exactly one case: the dashboard's only reference to `rate_limited` is the
  send-ratio panel's negative matcher `outcome!~"rate_limited"`. Under real negated-selection
  semantics that matcher *excludes* `rate_limited`, which would make the "every expected
  outcome matches ≥1 dashboard matcher" check wrongly flag a documented, intentional
  exception as a gap. Naming semantics treat that matcher as evidence the dashboard accounts
  for `rate_limited` (its author had to name it to carve it out) — verified against the
  actual rendered chart content, all 25 expected outcomes and all 6 distinct matchers
  cross-check cleanly both ways under this definition. The dead-regex check still excludes
  `!~` matchers, as originally specified.

## T6 — promtool rule checks in CI

**Why:** the PrometheusRule's exprs (including the `label_replace` reason extraction) are
never validated or exercised; a syntax error or broken regex ships silently.

**Files:** `.mise/config.toml` (+ `mise.lock`), `hack/promrules/main.go` (new),
`test/promrules/tests.yaml` (new), `.github/workflows/test.yml`.

**Steps:**
1. Pin promtool in `[tools]` via mise's ubi backend (promtool ships inside the prometheus
   release tarball; verify the exact mise syntax for selecting the `promtool` binary from
   `prometheus/prometheus`). Regenerate `mise.lock`. Check whether Renovate's mise manager
   bumps ubi-backend pins; if not, add a manual-bump comment.
2. `hack/promrules/main.go`: tiny filter — read the rendered PrometheusRule YAML on stdin,
   print its `.spec` as a plain Prometheus rules file on stdout (promtool doesn't understand
   the CRD wrapper).
3. `[tasks.promtool-rules]`:
   `helm template rel dist/chart -s templates/prometheus/prometheus-rule.yaml --set prometheusRule.enabled=true | go run ./hack/promrules > "$TMPDIR/rules.yaml"`,
   then `promtool check rules` and `promtool test rules test/promrules/tests.yaml` against it.
4. `test/promrules/tests.yaml`: synthetic `rss2discord_feed_operations_total` series firing
   each alert at the default `rateInterval: 15m` / `for: 15m`:
   - `outcome="fetch_error_not_found"` → `RSS2DiscordFeedFetchErrors` fires with
     `reason="not_found"` (proves the `label_replace`);
   - `outcome="rate_limited"` → `RSS2DiscordFeedRateLimited`;
   - `outcome="send_error_timeout"` → `RSS2DiscordFeedSendErrors` with `reason="timeout"`;
   - `outcome="render_error"` → assert **no** alert fires (encodes the deliberate
     dashboard-only decision).
5. Add `- run: mise run promtool-rules` as a step in the existing `Test` job in `test.yml`
   (after `mise run test`). **No new job name** → no skip-docs-checks change.

**Acceptance criteria:** `mise run promtool-rules` green locally and in CI; corrupting a
`label_replace` regex in the template makes `promtool test rules` fail.

**Verification:** `mise run promtool-rules` · `mise install --locked` from a clean checkout.

**Conflicts:** serialize with T8 (same workflow file) and T12 (`[tools]`/`mise.lock`).

## T7 — Chart golden-file snapshot tests

**Why:** `mise run helm-chart-refresh` must preserve hand-tuned templates (see CLAUDE.md for
the list); a refresh regression currently only shows up in `git diff` review. `helm lint`
(the only chart check in CI) validates syntax, not content.

**Approach decision:** golden-file `helm template` snapshots in a Go test — **not**
`helm unittest`, which is a helm *plugin* that can't be pinned in `[tools]` and would break
the mise single-source-of-truth rule. Golden tests reuse the same `helm template` mechanism
as T5/T6.

**Files:** `test/chart/chart_test.go` (new), `test/chart/testdata/*.golden.yaml` (new).
`test/**` is already codecov-ignored; the package runs under `mise run test` automatically
(it's not under `/e2e`).

**Steps:**
1. Table-driven cases, each a `helm template` render (fixed release name/namespace) scoped
   with `-s` to the five contract files: `templates/manager/manager.yaml`,
   `templates/metrics/controller-manager-metrics-service.yaml`,
   `templates/prometheus/controller-manager-metrics-monitor.yaml`,
   `templates/prometheus/prometheus-rule.yaml`,
   `templates/prometheus/grafana-dashboard.yaml`.
2. Value permutations to cover: defaults; `prometheus.enabled=true` with
   `scrapeNativeHistograms` both true and false (assert the
   `scrapeProtocols`/`scrapeClassicHistograms`/`scrapeNativeHistograms` block appears and
   disappears); `prometheusRule.enabled=true`; `grafanaDashboard.enabled=true` (both the
   ConfigMap and the GrafanaDashboard CR are emitted); `networkPolicy.enabled=true`;
   `metrics.secure=false` (port name flips to `http`).
3. Normalize volatile lines before comparing (`helm.sh/chart:`,
   `app.kubernetes.io/version:`) so release bumps don't churn goldens.
4. `-update` flag regenerates goldens. Document in the test header **and** in the
   `helm-chart-refresh` task comment: after an intentional chart change or a refresh, run
   `go test ./test/chart -update` and review the golden diff — this is the machine-checked
   version of the refresh task's "review the diff" step.
5. Fail (never skip) when helm is missing, same as T5.

**Acceptance criteria:** `mise run test` green; hand-corrupting the ServiceMonitor's
`scrapeProtocols` block fails the test; `mise run helm-chart-refresh` on a clean tree
leaves goldens passing (this is the actual regression test for the refresh).

**Verification:** `mise run test` · `mise run helm-chart-refresh && go test ./test/chart`.

## T8 — Cache envtest binaries in CI ✅ done

**Why:** `mise run test` downloads the envtest control-plane binaries
(`setup-envtest use ... --bin-dir bin`) on every Tests run; `.github/actions/setup-go-env`
only caches `~/.cache/go-build` and `~/go/pkg/mod`.

**Files:** `.github/workflows/test.yml` only. Deliberately **not** the shared composite
action — Lint/CodeQL don't need envtest binaries.

**Steps:** add an `actions/cache` step (same SHA pin as `setup-go-env/action.yml` uses)
before the `mise run test` step: `path: bin/k8s`,
`key: ${{ runner.os }}-envtest-${{ hashFiles('go.sum') }}` — the envtest version is derived
from `k8s.io/api` in `go.sum`, so the key rolls exactly when the version can change. No
`restore-keys` (exact-match only; a stale version dir would just sit unused).
`setup-envtest use` is a no-op when the version dir already exists.

**Acceptance criteria:** second CI run on the same `go.sum` shows a cache hit and no
envtest download in the `mise run test` log.

**Verification:** two consecutive workflow runs on the PR; `mise run test` locally
unaffected.

**Conflicts:** land before T6 (same file).

## T9 — Codecov patch status enforcing — land last

**Why:** with 96–99% package coverage the risk is regression, not absolute level; an
informational patch status never pushes back.

**Files:** `codecov.yml` only.

**Steps:** under `coverage.status.patch.default`, set `informational: false` and add
sane bounds (recommended: `target: 80%`, `threshold: 5%`). Leave `project` informational.

**Notes to carry in the PR body:**
- A red `codecov/patch` status only *blocks* merges if added to branch protection —
  recommend **not** doing that initially: the red X is visible pushback, and docs-only PRs
  (which never upload coverage) stay unblocked.
- Risk: refactors that move covered lines can flake patch coverage. Rollback is a
  one-line revert to `informational: true`.
- Sequenced last so the other PRs in this plan land under the current regime.

**Verification:** `curl --data-binary @codecov.yml https://codecov.io/validate` · the next
code PR after merge shows an enforcing patch status.

## T10 — synctest / clock-extraction spike — experimental, timeboxed

**Why:** the controller uses wall-clock `time.Now()` for backoff and status stamps, and
tests simulate elapsed time by editing RFC3339 strings. Go 1.25+ `testing/synctest` is
stable, and the repo already uses it (`internal/discord/ratelimiter_test.go`).

**Scope (deliberately small):** extract the `BackoffUntil` parse/compare logic and
`applyPermanentBackoff`'s timestamp math in `internal/controller/feedgroup_controller.go`
into pure helpers taking `now time.Time`, with table tests. Findings that bound the spike:
- `permanentBackoffDuration` is **already pure** and tested — no synctest needed for it.
- **No** `Clock` field on the reconciler, and **no** envtest/Ginkgo goroutines inside a
  synctest bubble (controller-runtime doesn't belong there).
- A written "no-go beyond pure helpers" conclusion posted to issue 106 is an acceptable
  outcome for this task.

**Acceptance criteria:** behavior-preserving refactor — the existing ~43
timestamp-manipulating assertions in `processfeed_test.go` / `feedgroup_controller_test.go`
pass unmodified or with mechanical-only updates.

**Verification:** `mise run test` · `mise run lint`.

## T11 — goleak TestMain for `internal/rss` + `internal/discord` — experimental

**Why:** both packages spawn HTTP-client goroutines worth leak-checking; both are plain Go
test packages with no `TestMain` today. `go.uber.org/goleak` v1.3.0 is already in `go.sum`
as a transitive dependency. Scope excludes `internal/controller` (controller-runtime leaks
knowingly — envtest suite is a poor fit).

**Files:** `internal/rss/main_test.go`, `internal/discord/main_test.go` (new);
`go.mod`/`go.sum` (goleak becomes a direct dependency — run `go mod tidy` and commit, or
the tidiness gate in `test.yml` fails the PR).

**Steps:** `func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }` in each package.
Expect to fix tests rather than add ignores: close `httptest` servers, call
`CloseIdleConnections` where clients linger. Only if unavoidable, a targeted
`goleak.IgnoreTopFunction` for stdlib pollers, with a comment justifying it.

**Acceptance criteria:** `mise run test` green and stable
(`go test -count=5 ./internal/rss ./internal/discord` shows no flakes).

**Verification:** `mise run test` · `go test -count=5 ./internal/rss ./internal/discord`.

## T12 — Mutation-testing experiment — experimental, gated

**Why:** at 96–99% line coverage the open question is assertion strength, not reach.
Surviving mutants answer it.

**Phase 1 (this task's PR):** pin `go-gremlins` via mise's ubi backend (+ regenerate
`mise.lock`), add `[tasks.mutation]` scoped to `./internal/...` (exclude
`zz_generated*.go`), run it **manually once**, and post the surviving-mutant summary as a
comment on issue 106.

**Abort criteria (state the finding on the issue and stop):** gremlins doesn't build/run on
go 1.26; runtime exceeds ~30 minutes; or the report is all noise. Try `ooze` only if it's a
trivial swap.

**Phase 2 (separate PR, only if the phase-1 report was actionable):** `mutation.yml`
scheduled workflow (cron + `workflow_dispatch`, artifact-upload of the report,
never PR-triggered → no skip-docs-checks change).

**Verification:** `mise run mutation` completes · `mise install --locked` clean.

**Conflicts:** serialize with T6 (`[tools]` + `mise.lock`).

## T13 — ClusterFuzzLite evaluation — deferred

Do not start yet. Entry criteria: T1's scheduled fuzzing has ≥4 runs of history, and the
cumulative corpus either found a crasher or grew meaningfully. Then evaluate ClusterFuzzLite
for PR-time fuzzing of changed code — noting up front that it requires OSS-Fuzz-style build
scripts and Docker, and would be the only non-mise CI path in the repo (tension with ground
rule 2 — needs explicit maintainer approval before any implementation).

---

## Appendix: pitfalls quick reference

1. The sample CR's `apiVersion` is broken until T3 lands — any task applying samples
   before that hits "no matches for kind".
2. `go test -fuzz` = one target per invocation; controller-package fuzz runs need
   `-run '^$'` or the envtest suite boots.
3. Corpus vs crashers: cumulative corpus lives in `$(go env GOCACHE)/fuzz` (cache it);
   crashers land in `<pkg>/testdata/fuzz/<Target>/` (artifact-upload, then commit as seeds).
4. `actions/cache` keys are immutable — cumulative caches need a `run_id`-suffixed key with
   a prefix `restore-keys`; scheduled runs use the default branch's cache scope, which is
   what you want.
5. Persistent-failure Event: reason `FetchFailed`, action `RetriesExhausted`; fires exactly
   once (`RetryCount == maxRetries`) — never assert it re-fires.
6. `resolveWebhookURL` runs before any fetch — failure-path e2e still needs a real Secret.
7. Prometheus label regexes are fully anchored RE2 — anchor with `^(?:...)$` when
   cross-checking in Go, and handle the negative matcher `outcome!~"rate_limited"`
   explicitly.
8. `render_error` deliberately has no alert — an explicit exception in T5/T6, not a bug to
   "fix".
9. helm-rendered output embeds chart/app versions — scrub `helm.sh/chart:` /
   `app.kubernetes.io/version:` lines in golden tests.
10. T5/T6/T7 make helm/promtool hard dependencies of the test tasks — fail with an
    actionable message, never skip.
11. Every `[tools]` change regenerates `mise.lock`, or CI's `mise install --locked` fails.
12. Requeue jitter sits on top of `retryInterval` — keep e2e `Eventually` windows ≥ 60s.
