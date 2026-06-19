# rss2discord-operator

A Kubernetes operator that automatically posts RSS feed updates to Discord channels via webhooks. Manage your RSS feeds declaratively with Kubernetes CRDs!

## Features

- 📰 **RSS Feed Management**: Define RSS feeds via Kubernetes CRDs
- 🔗 **Discord Integration**: Send feed updates directly to Discord webhooks
- 🎯 **Flexible Filtering**: Filter feed entries by regex patterns or keywords
- 🎨 **Custom Formatting**: Customize Discord message templates
- ⏱️ **Configurable Intervals**: Control how often feeds are checked
- 🔄 **Automatic Retries**: Built-in retry logic for failed operations
- 📊 **Kubernetes Native**: Uses RBAC, status conditions, and Kubernetes logging
- 🐳 **Multi-Architecture**: Supports Linux ARM64, AMD64, s390x, and ppc64le

## Quick Start

### Prerequisites

- Kubernetes 1.26+ cluster
- Helm 3.0+ (for Helm-based installation)
- A Discord webhook URL (from a channel's webhooks settings)

### Installation Options

#### Option 1: Helm Chart (Recommended)

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace
```

Or upgrade an existing release:

```bash
helm upgrade rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace
```

#### Option 2: kubectl (YAML Bundle)

```bash
kubectl apply -f dist/install.yaml
```

This creates the `rss2discord-operator-system` namespace and deploys the operator.

#### Option 3: Make Targets

```bash
# Using Helm
IMG=my-registry/rss2discord-operator:v0.1.0 make helm-deploy

# Using kubectl
IMG=my-registry/rss2discord-operator:v0.1.0 make deploy
```

### Create a FeedGroup

1. **Create a Discord webhook secret** in your namespace:

```bash
kubectl create secret generic discord-webhook \
  -n default \
  --from-literal=url='https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_TOKEN'
```

2. **Create a FeedGroup CRD**:

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

3. **Apply the resource**:

```bash
kubectl apply -f feedgroup.yaml
```

4. **Monitor the operator**:

```bash
kubectl logs -n rss2discord-operator-system deployment/rss2discord-operator-controller-manager -f
```

## Configuration

### FeedGroup Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `discordWebhookSecretRef` | `SecretKeySelector` | Required | Reference to Secret containing Discord webhook URL |
| `interval` | `string` | `"30m"` | Duration between feed checks (e.g., "30m", "1h") |
| `retries` | `int` | `3` | Number of retries for failed operations |
| `retryInterval` | `string` | `"5m"` | Duration between retry attempts |
| `format` | `string` | See below | Template for Discord messages |
| `feeds` | `[]Feed` | Required | List of RSS feeds to monitor |

### Feed Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rssUrl` | `string` | Required | URL of the RSS feed |
| `filter` | `*Filter` | Optional | Filter rules for feed entries |
| `format` | `string` | Optional | Override group-level message format for this feed |
| `paused` | `bool` | `false` | Temporarily stop processing this feed |

### Filter Configuration

| Field | Type | Description |
|-------|------|-------------|
| `regex` | `string` | Regular expression to match against title/description |
| `keywords` | `[]string` | List of keywords to match (OR logic) |

### Message Template

Default template:
```
**{{.Title}}**
{{.Description}}
[Read more]({{.Link}})
```

Available placeholders:
- `{{.Title}}` - Entry title
- `{{.Description}}` - Entry description
- `{{.Link}}` - Entry URL
- `{{.Published}}` - Publication timestamp

## Development

### Prerequisites

- Go 1.23+
- Make
- Docker (for building container images)
- Kind (optional, for local testing)

### Build & Test

```bash
# Build the manager binary
make build

# Run tests
make test

# Run linter
make lint
make lint-fix

# Run e2e tests (requires Kind cluster)
make test-e2e
```

### Generate Manifests

After modifying CRD types or RBAC markers:

```bash
make manifests   # Generate CRDs and RBAC
make generate    # Generate DeepCopy methods
```

### Build & Push Container Image

```bash
export IMG=my-registry/rss2discord-operator:v0.1.0
make docker-build
make docker-push
```

### Deploy Locally

```bash
# Using Kustomize
IMG=my-registry/rss2discord-operator:v0.1.0 make deploy

# Using Helm
IMG=my-registry/rss2discord-operator:v0.1.0 make helm-deploy
```

## Project Structure

```
.
├── api/v1alpha1/               # FeedGroup CRD types
│   ├── feedgroup_types.go
│   └── groupversion_info.go
├── internal/
│   ├── controller/             # Reconciliation logic
│   │   └── feedgroup_controller.go
│   ├── discord/                # Discord webhook client
│   │   └── client.go
│   └── rss/                    # RSS feed client
│       └── client.go
├── config/                     # Kubernetes manifests
│   ├── crd/                    # CRD definitions
│   ├── rbac/                   # RBAC rules
│   ├── manager/                # Deployment config
│   ├── default/                # Kustomize overlays
│   └── samples/                # Example CRs
├── dist/                       # Generated output
│   ├── install.yaml            # All-in-one YAML bundle
│   └── chart/                  # Helm chart
├── test/e2e/                   # End-to-end tests
├── cmd/main.go                 # Entry point
├── Dockerfile                  # Container image
└── Makefile                    # Build targets
```

## Helm Chart

The Helm chart is located in `dist/chart/` and provides a production-ready way to deploy the operator.

### Helm Chart Values

Key customizable values in `dist/chart/values.yaml`:

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

### Helm Commands

```bash
# Deploy with custom values
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace \
  --set manager.image.tag=v0.1.0 \
  --set manager.replicaCount=2

# Check status
helm status rss2discord-operator -n rss2discord-operator-system

# View release history
helm history rss2discord-operator -n rss2discord-operator-system

# Rollback to previous version
helm rollback rss2discord-operator -n rss2discord-operator-system

# Uninstall
helm uninstall rss2discord-operator -n rss2discord-operator-system
```

For more options, see `dist/chart/values.yaml`.

## Troubleshooting

### Check operator logs

```bash
kubectl logs -n rss2discord-operator-system deployment/rss2discord-operator-controller-manager -f
```

### Check FeedGroup status

```bash
kubectl describe feedgroup my-feedgroup -n default
```

View detailed status conditions:

```bash
kubectl get feedgroup my-feedgroup -n default -o yaml
```

### Common Issues

**Issue**: FeedGroup shows `LastError: webhook - discord webhook URL is empty`
- **Solution**: Verify the Discord webhook secret exists and the key is correct in `discordWebhookSecretRef`

**Issue**: FeedGroup not updating
- **Solution**: Check the `interval` setting and operator logs for errors

**Issue**: Messages not formatting correctly
- **Solution**: Ensure template placeholders match available fields (`.Title`, `.Description`, `.Link`, `.Published`)

## References

- [Kubebuilder Documentation](https://book.kubebuilder.io)
- [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Discord Webhook Documentation](https://discord.com/developers/docs/resources/webhook)

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

## Contributing

Contributions are welcome! Please feel free to submit pull requests.