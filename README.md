# rss2discord-operator

[![codecov](https://codecov.io/gh/maverickd650/rss2discord-operator/graph/badge.svg)](https://codecov.io/gh/maverickd650/rss2discord-operator)

A Kubernetes operator that watches RSS feeds and posts new entries to Discord via webhooks. Feeds are configured declaratively with a `FeedGroup` custom resource.

> **Note:** This project is vibecoded — built quickly with heavy AI assistance, not deeply hardened or extensively reviewed. It works for personal/small-scale use, but read the code before trusting it with anything important.

## Features

- Define RSS feeds as Kubernetes CRDs
- Post updates to Discord webhooks
- Filter entries by regex or keywords
- Customize the message format per feed group or per feed
- Render entries as native Discord embeds (colored bubble, thumbnail, author/footer) instead of plain text
- Post into forum channels, either as a new thread per entry or into an existing thread
- Override the webhook's display name/avatar per feed group
- Conditional GET (ETag / If-Modified-Since) on RSS fetches — skips re-downloading and re-parsing unchanged feeds
- Configurable check interval and retry behavior

## Installing

Requires a Kubernetes 1.26+ cluster and a Discord webhook URL.

### Helm

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace
```

### kubectl

```bash
kubectl apply -f dist/install.yaml
```

### Make

```bash
IMG=my-registry/rss2discord-operator:v0.1.0 mise run deploy
# or
IMG=my-registry/rss2discord-operator:v0.1.0 mise run helm-deploy
```

## Usage

1. Create a secret with your Discord webhook URL:

```bash
kubectl create secret generic discord-webhook \
  -n default \
  --from-literal=url='https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_TOKEN'
```

2. Create a `FeedGroup`:

```yaml
apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata:
  name: tech-news
  namespace: default
spec:
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  interval: "30m"
  retries: 3
  retryInterval: "5m"
  format: |
    **{{.Title}}**
    {{.Description}}
    [Read more]({{.Link}})
  feeds:
    - rssUrl: "https://news.ycombinator.com/rss"
      filter:
        keywords:
          - kubernetes
          - golang
    - rssUrl: "https://www.reddit.com/r/golang/.rss"
      paused: false
```

To render entries as embeds (with a colored bubble and thumbnail) instead of plain text, and to post each entry as a new forum thread:

```yaml
spec:
  embed:
    enabled: true
    color: "#5865F2"
    footerText: "via tech-news"
  feeds:
    - rssUrl: "https://news.ycombinator.com/rss"
      forumThreadName: "{{.Title}}"
```

```bash
kubectl apply -f feedgroup.yaml
```

3. Watch it work:

```bash
kubectl logs -n rss2discord-operator-system deployment/rss2discord-operator-controller-manager -f
```

## Configuration reference

### FeedGroup spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `discordWebhookSecretRef` | `SecretKeySelector` | required | Secret containing the Discord webhook URL |
| `interval` | `string` | `30m` | How often to check feeds |
| `retries` | `int` | `3` | Retries for failed operations |
| `retryInterval` | `string` | `5m` | Delay between retries |
| `format` | `string` | see below | Discord message template (used when `embed` is not enabled) |
| `embed` | `*Embed` | optional | Default embed config for all feeds in the group |
| `username` | `string` | optional | Overrides the webhook's display name (Discord rejects names containing "clyde" or "discord", or over 80 characters) |
| `avatarURL` | `string` | optional | Overrides the webhook's avatar |
| `feeds` | `[]Feed` | required | RSS feeds to monitor |

### Feed

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rssUrl` | `string` | required | RSS feed URL |
| `filter` | `*Filter` | optional | Filter rules for entries |
| `format` | `string` | optional | Overrides the group's format for this feed |
| `embed` | `*Embed` | optional | Overrides the group's embed config for this feed |
| `forumThreadName` | `string` | optional | Template for a new forum post's title; set on FeedGroups whose webhook targets a forum channel |
| `forumThreadID` | `string` | optional | Posts into an existing forum thread instead of creating a new one (takes precedence over `forumThreadName`) |
| `paused` | `bool` | `false` | Stop processing this feed without removing it |

### Filter

| Field | Type | Description |
|-------|------|-------------|
| `regex` | `string` | Regex matched against title/description |
| `keywords` | `[]string` | Keywords matched against title/description (OR) |

### Embed

When `enabled`, a feed's messages are sent as a native Discord embed (the colored bubble UI) instead of plain text. The embed's title/link/timestamp come directly from the entry; only the description is templated.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | `bool` | `false` | Render messages as embeds instead of plain text |
| `color` | `string` | none | Side-bar color, as hex (`#5865F2` or `5865F2`) |
| `descriptionFormat` | `string` | `{{.Description}}` | Template for the embed description |
| `authorName` | `string` | optional | Shown on the embed's author line |
| `footerText` | `string` | optional | Shown in the embed's footer |

If the feed entry has a lead image (an RSS `<enclosure>` or Media RSS `<media:thumbnail>`/`<media:content>`, or an Atom `<link rel="enclosure">`), it's attached automatically as the embed's thumbnail.

### Message template

Default (used only when `embed.enabled` is false):

```
**{{.Title}}**
{{.Description}}
[Read more]({{.Link}})
```

Available fields: `.Title`, `.Description`, `.Link`, `.Published`.

### Forum channels

If `discordWebhookSecretRef` points at a webhook created for a forum channel, set `forumThreadName` on a feed to create a new forum post per entry (templated, same placeholders as `format`), or `forumThreadID` to post into one specific existing thread instead.

## Development

Tooling is managed by [mise](https://mise.jdx.dev) — it pins every tool version (Go, golangci-lint, controller-gen, kustomize, helm, kind, etc.) in [`.mise/config.toml`](.mise/config.toml), so local development matches CI exactly. Docker is the only host prerequisite.

```bash
curl https://mise.run | sh   # one-time: install mise
mise install                 # install the pinned toolchain
mise tasks                   # list available tasks
```

```bash
mise run build       # build the manager binary
mise run test        # unit tests
mise run lint        # lint
mise run lint-fix    # lint with autofix
mise run test-e2e    # e2e tests, needs a Kind cluster
```

After changing CRD types or RBAC markers:

```bash
mise run manifests   # regenerate CRDs and RBAC
mise run generate    # regenerate DeepCopy methods
```

Build and push an image:

```bash
export IMG=my-registry/rss2discord-operator:v0.1.0
mise run docker-build
mise run docker-push
```

## Project layout

```
api/v1alpha1/          FeedGroup CRD types
internal/controller/   Reconciliation logic
internal/discord/      Discord webhook client
internal/rss/          RSS feed client
config/                Kubernetes manifests (CRDs, RBAC, kustomize)
dist/                  Generated install.yaml and Helm chart
test/e2e/              End-to-end tests
cmd/main.go            Entry point
```

## Helm chart

Located in `dist/chart/`. Key values in `dist/chart/values.yaml`:

```yaml
manager:
  image:
    repository: rss2discord-operator
    tag: latest
  resources:
    limits:
      cpu: 500m
      memory: 128Mi
    requests:
      cpu: 100m
      memory: 64Mi
```

Common commands:

```bash
helm status rss2discord-operator -n rss2discord-operator-system
helm history rss2discord-operator -n rss2discord-operator-system
helm rollback rss2discord-operator -n rss2discord-operator-system
helm uninstall rss2discord-operator -n rss2discord-operator-system
```

## Troubleshooting

Check logs:

```bash
kubectl logs -n rss2discord-operator-system deployment/rss2discord-operator-controller-manager -f
```

Check FeedGroup status:

```bash
kubectl describe feedgroup my-feedgroup -n default
kubectl get feedgroup my-feedgroup -n default -o yaml
```

Common issues:

- `LastError: webhook - discord webhook URL is empty` — the secret or key in `discordWebhookSecretRef` is wrong.
- FeedGroup not updating — check `interval` and the operator logs.
- Messages formatted wrong — check template placeholders against `.Title`, `.Description`, `.Link`, `.Published`.

## References

- [Kubebuilder Documentation](https://book.kubebuilder.io)
- [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Discord Webhook Documentation](https://discord.com/developers/docs/resources/webhook)

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).
