# Quick Start Guide

## 🚀 Deploy the Operator

### Option 1: Helm (Recommended)

```bash
# Deploy to your cluster
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace

# Verify deployment
kubectl get pods -n rss2discord-operator-system
```

### Option 2: kubectl

```bash
# Deploy using the generated YAML bundle
kubectl apply -f dist/install.yaml

# Verify deployment
kubectl get pods -n rss2discord-operator-system
```

### Option 3: Using Make

```bash
# Build and deploy your own image
export IMG=myregistry/rss2discord-operator:v0.1.0
make docker-build docker-push
make helm-deploy IMG=$IMG
```

## 📝 Create a FeedGroup Resource

### Step 1: Create Discord Webhook Secret

Get your Discord webhook URL from a channel's webhook settings, then:

```bash
kubectl create secret generic discord-webhook \
  -n default \
  --from-literal=url='https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_TOKEN'
```

### Step 2: Create FeedGroup Resource

Save this to `my-feedgroup.yaml`:

```yaml
apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata:
  name: tech-news
  namespace: default
spec:
  # Reference to Discord webhook secret
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  
  # Check feeds every 30 minutes
  interval: "30m"
  
  # Retry failed operations up to 3 times
  retries: 3
  retryInterval: "5m"
  
  # Default message format (customize per feed if needed)
  format: |
    **{{.Title}}**
    {{.Description}}
    [Read more]({{.Link}})
  
  # List of RSS feeds to monitor
  feeds:
    # Hacker News
    - rssUrl: "https://news.ycombinator.com/rss"
      filter:
        keywords:
          - kubernetes
          - golang
    
    # Go subreddit
    - rssUrl: "https://www.reddit.com/r/golang/.rss"
      paused: false
```

Apply it:

```bash
kubectl apply -f my-feedgroup.yaml
```

### Step 3: Monitor the Operator

```bash
# View operator logs
kubectl logs -n rss2discord-operator-system \
  deployment/rss2discord-operator-controller-manager -f

# Check FeedGroup status
kubectl describe feedgroup tech-news -n default

# See detailed status (YAML format)
kubectl get feedgroup tech-news -n default -o yaml
```

## 🛠️ Development

### Build & Test

```bash
# Run tests
make test

# Build binary
make build

# Run linter
make lint-fix
```

### Modify API Types

If you edit `api/v1alpha1/feedgroup_types.go`:

```bash
# Regenerate CRDs and RBAC
make manifests

# Regenerate DeepCopy methods
make generate

# Run tests to verify
make test
```

### Build & Push Custom Image

```bash
export IMG=myregistry/rss2discord-operator:v0.1.0

# Build locally
make docker-build IMG=$IMG

# Push to registry
make docker-push IMG=$IMG
```

## 📦 Deployment Management

### Check Helm Release Status

```bash
helm status rss2discord-operator -n rss2discord-operator-system
```

### View Helm Release History

```bash
helm history rss2discord-operator -n rss2discord-operator-system
```

### Rollback to Previous Version

```bash
helm rollback rss2discord-operator -n rss2discord-operator-system
```

### Uninstall

```bash
# Using Helm
helm uninstall rss2discord-operator -n rss2discord-operator-system

# Or using kubectl
kubectl delete -f dist/install.yaml
```

## 🔍 Troubleshooting

### Check if operator is running

```bash
kubectl get pods -n rss2discord-operator-system
```

### View operator logs

```bash
kubectl logs -n rss2discord-operator-system \
  deployment/rss2discord-operator-controller-manager -f
```

### Check FeedGroup events

```bash
kubectl describe feedgroup <name> -n <namespace>
```

### Common Error: "discord webhook URL is empty"

- Verify the secret exists: `kubectl get secret discord-webhook -n default`
- Check the URL is correct: `kubectl get secret discord-webhook -n default -o yaml`
- Ensure the secret key matches `discordWebhookSecretRef.key` in the FeedGroup

### Common Error: "webhook URL not found in secret"

- Check the secret name and namespace in your FeedGroup
- Verify the key name matches exactly (usually `url`)

## 🎯 Next Steps

1. **Customize Message Format**: Update the `format` field in FeedGroup to customize Discord messages
2. **Add More Feeds**: Add more items to the `feeds` array in your FeedGroup
3. **Set Appropriate Intervals**: Adjust `interval` based on how often you want to check feeds
4. **Use Filters**: Add `filter` with `regex` or `keywords` to only notify about relevant entries
5. **Monitor with Prometheus**: The operator exposes metrics on port 8443

## 📚 More Information

- See [README.md](README.md) for complete documentation
- Kubebuilder docs: https://book.kubebuilder.io
- Operator pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
