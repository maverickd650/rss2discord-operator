# rss2discord-operator

A Kubernetes operator that watches RSS feeds and posts new entries to Discord via webhooks. Feeds are configured declaratively with a `FeedGroup` custom resource.

> **Note:** This project is vibecoded — built quickly with heavy AI assistance, not deeply hardened or extensively reviewed. It works for personal/small-scale use, but read the code before trusting it with anything important.

## Features

- Define RSS feeds as Kubernetes CRDs
- Post updates to Discord webhooks
- Filter entries by regex or keywords
- Customize the message format per feed group or per feed
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
IMG=my-registry/rss2discord-operator:v0.1.0 make deploy
# or
IMG=my-registry/rss2discord-operator:v0.1.0 make helm-deploy
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
| `format` | `string` | see below | Discord message template |
| `feeds` | `[]Feed` | required | RSS feeds to monitor |

### Feed

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rssUrl` | `string` | required | RSS feed URL |
| `filter` | `*Filter` | optional | Filter rules for entries |
| `format` | `string` | optional | Overrides the group's format for this feed |
| `paused` | `bool` | `false` | Stop processing this feed without removing it |

### Filter

| Field | Type | Description |
|-------|------|-------------|
| `regex` | `string` | Regex matched against title/description |
| `keywords` | `[]string` | Keywords matched against title/description (OR) |

### Message template

Default:

```
**{{.Title}}**
{{.Description}}
[Read more]({{.Link}})
```

Available fields: `.Title`, `.Description`, `.Link`, `.Published`.

## Development

Requires Go 1.23+, Make, and Docker.

```bash
make build       # build the manager binary
make test        # unit tests
make lint        # lint
make lint-fix    # lint with autofix
make test-e2e    # e2e tests, needs a Kind cluster
```

After changing CRD types or RBAC markers:

```bash
make manifests   # regenerate CRDs and RBAC
make generate    # regenerate DeepCopy methods
```

Build and push an image:

```bash
export IMG=my-registry/rss2discord-operator:v0.1.0
make docker-build
make docker-push
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
