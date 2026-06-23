# Changelog

## [0.6.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.6.0...v0.6.1) (2026-06-23)


### Bug Fixes

* **chart:** move ServiceMonitor native histogram fields to spec top level ([#61](https://github.com/maverickd650/rss2discord-operator/issues/61)) ([a8cb30b](https://github.com/maverickd650/rss2discord-operator/commit/a8cb30b18c4beed468c1d74648739dfab57b60f9))

## [0.6.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.5.0...v0.6.0) (2026-06-23)


### Features

* add Prometheus native histogram support for request latency ([#58](https://github.com/maverickd650/rss2discord-operator/issues/58)) ([dec2ffb](https://github.com/maverickd650/rss2discord-operator/commit/dec2ffb177ec7670e78d27a36897ba6834d7989d))
* redesign Grafana dashboard and ship it via grafana-operator ([#57](https://github.com/maverickd650/rss2discord-operator/issues/57)) ([a0e69c1](https://github.com/maverickd650/rss2discord-operator/commit/a0e69c10b1ef57505da29ec430a7619e96bb05ec))


### Bug Fixes

* increase test coverage ([#54](https://github.com/maverickd650/rss2discord-operator/issues/54)) ([c6c4cd0](https://github.com/maverickd650/rss2discord-operator/commit/c6c4cd03a692c84102b176338f37906d4c2780aa))

## [0.5.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.3...v0.5.0) (2026-06-23)


### Features

* add request latency and delivery freshness metrics ([#50](https://github.com/maverickd650/rss2discord-operator/issues/50)) ([503d475](https://github.com/maverickd650/rss2discord-operator/commit/503d47565617ede56bd3e2ffd85a3a2121578baa))


### Bug Fixes

* deliver newest entries for date-less feeds and harden rate-limit/status handling ([#48](https://github.com/maverickd650/rss2discord-operator/issues/48)) ([8adaa4f](https://github.com/maverickd650/rss2discord-operator/commit/8adaa4f967a18b11ac474246558a8e68d008f18b))

## [0.4.3](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.2...v0.4.3) (2026-06-22)


### Bug Fixes

* flag the LastChecked 304 status-write fix for release-please ([#45](https://github.com/maverickd650/rss2discord-operator/issues/45)) ([cc3f1c2](https://github.com/maverickd650/rss2discord-operator/commit/cc3f1c219152b62e6625ada3dc1dc7d64a2cdd77))

## [0.4.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.1...v0.4.2) (2026-06-21)


### Bug Fixes

* strip trailing "Continue reading" boilerplate, remove dead Embed.ImageURL ([#42](https://github.com/maverickd650/rss2discord-operator/issues/42)) ([04fc026](https://github.com/maverickd650/rss2discord-operator/commit/04fc0269b03fa8d284d5fcc15be8587578627ade))

## [0.4.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.4.0...v0.4.1) (2026-06-21)


### Bug Fixes

* fall back to catch-up when LastSeenEntry scrolls out of the feed window ([#38](https://github.com/maverickd650/rss2discord-operator/issues/38)) ([8af17de](https://github.com/maverickd650/rss2discord-operator/commit/8af17de3b212d54cf63b7c92adf3ef5306c58fec))
* handle non-UTF-8 feeds, link/title churn, relative Atom links, and HTML in titles ([#41](https://github.com/maverickd650/rss2discord-operator/issues/41)) ([d048fe9](https://github.com/maverickd650/rss2discord-operator/commit/d048fe96d2a281ea33ebb521ff41314cc1e6ff53))

## [0.4.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.3.0...v0.4.0) (2026-06-21)


### ⚠ BREAKING CHANGES

* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (2 → 3) ([#26](https://github.com/maverickd650/rss2discord-operator/issues/26))

### Features

* add fuzz test for feed parser, expose entry author/categories ([#34](https://github.com/maverickd650/rss2discord-operator/issues/34)) ([023ac5a](https://github.com/maverickd650/rss2discord-operator/commit/023ac5a5ad1b2281c9a310b3e18c915329610817))
* **chart:** add Grafana dashboard + PrometheusRule, shorten resource names ([#35](https://github.com/maverickd650/rss2discord-operator/issues/35)) ([f33fd1b](https://github.com/maverickd650/rss2discord-operator/commit/f33fd1b329ac9a33227396589933c20977be1aaa))
* **container:** Update image ghcr.io/devcontainers/features/docker-in-docker (2 → 3) ([#26](https://github.com/maverickd650/rss2discord-operator/issues/26)) ([492a51c](https://github.com/maverickd650/rss2discord-operator/commit/492a51cad1d7d36ddb17538fb75070e5f6bad714))


### Bug Fixes

* **chart:** add OCI source annotation so Renovate can find release notes ([#32](https://github.com/maverickd650/rss2discord-operator/issues/32)) ([b88fa0f](https://github.com/maverickd650/rss2discord-operator/commit/b88fa0f725c459c799ae4f3acf07c0923a733b02))
* **ci:** address zizmor findings in release workflow ([#30](https://github.com/maverickd650/rss2discord-operator/issues/30)) ([38f6f36](https://github.com/maverickd650/rss2discord-operator/commit/38f6f36279d219ea49795737dd3babc9872997cd))

## [0.3.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.2...v0.3.0) (2026-06-20)


### Features

* add per-outcome metrics and persistent-failure Events for feed processing ([#15](https://github.com/maverickd650/rss2discord-operator/issues/15)) ([dfbc96b](https://github.com/maverickd650/rss2discord-operator/commit/dfbc96b66fc143bfe125d148c8d001be4b1b59c3))
* skip re-fetching unchanged RSS feeds via conditional GET ([#13](https://github.com/maverickd650/rss2discord-operator/issues/13)) ([15bc767](https://github.com/maverickd650/rss2discord-operator/commit/15bc767398d28ce71ceb6731aaff58d8fbf155a7))
* support Discord embeds, forum channels, and webhook branding ([#12](https://github.com/maverickd650/rss2discord-operator/issues/12)) ([7f0438d](https://github.com/maverickd650/rss2discord-operator/commit/7f0438d1ed16f7afcbf0b50595afa5a409d1c499))


### Bug Fixes

* bound feed fan-out, tighten SSRF guard, sanitize embed URLs, stop infinite render retries ([#20](https://github.com/maverickd650/rss2discord-operator/issues/20)) ([0ebbd5b](https://github.com/maverickd650/rss2discord-operator/commit/0ebbd5b23275df06051f26f6566c62baa13dd809))
* cap initial backlog catch-up and strip HTML from Discord messages ([#10](https://github.com/maverickd650/rss2discord-operator/issues/10)) ([17bb2f5](https://github.com/maverickd650/rss2discord-operator/commit/17bb2f529a6e49c83ee49594b7a7d022904b783f))
* **chart:** restore Interval/RetryInterval validation in Helm CRD ([#22](https://github.com/maverickd650/rss2discord-operator/issues/22)) ([92934c1](https://github.com/maverickd650/rss2discord-operator/commit/92934c1e836f23c31806f6440aeffec52e67dcfa))
* prevent dropped entries from conditional GETs and prune stale feed status ([#14](https://github.com/maverickd650/rss2discord-operator/issues/14)) ([8e06824](https://github.com/maverickd650/rss2discord-operator/commit/8e06824cf03cf77e7f32265fd82192cd3fe76145))
* remove unused admission webhook scaffold from manager ([#16](https://github.com/maverickd650/rss2discord-operator/issues/16)) ([6c2317e](https://github.com/maverickd650/rss2discord-operator/commit/6c2317e7be39f034c63a329d96489bcec60bc8c0))
* skip no-op status writes and reject malformed Interval/RetryInterval at apply time ([#17](https://github.com/maverickd650/rss2discord-operator/issues/17)) ([4e06d44](https://github.com/maverickd650/rss2discord-operator/commit/4e06d44aedd11a283c640d6a8bd847a4ac4a6865))

## [0.2.2](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.1...v0.2.2) (2026-06-20)


### Bug Fixes

* disable informer cache for Secrets to match get-only RBAC ([#8](https://github.com/maverickd650/rss2discord-operator/issues/8)) ([b8a1271](https://github.com/maverickd650/rss2discord-operator/commit/b8a1271f6c9940e81fa5097555d6ff81c5da8900))

## [0.2.1](https://github.com/maverickd650/rss2discord-operator/compare/v0.2.0...v0.2.1) (2026-06-20)


### Bug Fixes

* **chart:** point default image at ghcr.io drop CRDs by default on uninstall ([#6](https://github.com/maverickd650/rss2discord-operator/issues/6)) ([ada6b97](https://github.com/maverickd650/rss2discord-operator/commit/ada6b9788f72d5fe8cb8e67f43ea6a7faa243109))

## [0.2.0](https://github.com/maverickd650/rss2discord-operator/compare/v0.1.0...v0.2.0) (2026-06-20)


### Features

* **init:** initial commit ([0a6f662](https://github.com/maverickd650/rss2discord-operator/commit/0a6f66260e153d241b5adc3563aa35623825da44))
* **perf:** performance and optimisation pass ([#4](https://github.com/maverickd650/rss2discord-operator/issues/4)) ([5d25cc7](https://github.com/maverickd650/rss2discord-operator/commit/5d25cc74c328276571c92271de62d89db1b3e5b7))


### Bug Fixes

* **ci:** efficiency ([#5](https://github.com/maverickd650/rss2discord-operator/issues/5)) ([ba6e7cb](https://github.com/maverickd650/rss2discord-operator/commit/ba6e7cb9c4aea889126a9105fdd775e9cf189a82))
