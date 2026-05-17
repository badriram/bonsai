# Using Bonsai — app developer guide

This is the surface a developer touches. If you're the operator who set Bonsai
up, read [`operator.md`](operator.md).

## The whole API

```sh
bonsai grow      # provision or reconcile a cluster
bonsai status    # show what's running
bonsai logs      # tail workload logs
```

That's it. Updates (k3s, OS, operators, AMI rotation) happen autonomously —
you never touch them.

## Install

Single static binary from GitHub Releases:

```sh
curl -sSL https://github.com/badriram/bonsai/releases/latest/download/bonsai-$(uname -s)-$(uname -m) -o /usr/local/bin/bonsai
chmod +x /usr/local/bin/bonsai
```

Or via Homebrew (planned):

```sh
brew install badriram/tap/bonsai
```

## `bonsai grow`

Provisions a cluster if it doesn't exist, reconciles drift if it does. **Safe
to run on every deploy.**

```sh
bonsai grow \
  --provider aws \
  --name my-app \
  --env prod \
  --region us-east-1 \
  --workers 1
```

Flags:

| Flag         | Default     | Notes                                |
|--------------|-------------|--------------------------------------|
| `--provider` | `aws`       | `aws` only in Phase 1                |
| `--name`     | _required_  | App name; namespacing for resources  |
| `--env`      | `dev`       | `dev` / `staging` / `prod` etc.      |
| `--region`   | `us-east-1` | Cloud region                         |
| `--workers`  | `1`         | Initial worker count                 |

Outputs are written to AWS Parameter Store under `/bonsai/<name>/<env>/`:

```
/bonsai/<name>/<env>/cluster_endpoint
/bonsai/<name>/<env>/kubeconfig
/bonsai/<name>/<env>/postgres_url
/bonsai/<name>/<env>/kv_url
/bonsai/<name>/<env>/token          # k3s join token (internal use)
```

Stdout echoes the same:

```
CLUSTER_ENDPOINT  https://ec2-xx.compute.amazonaws.com:6443
KUBECONFIG        ssm:///bonsai/my-app/prod/kubeconfig
POSTGRES_URL      postgres://app:***@postgres-rw.my-app.svc:5432/app
KV_URL            redis://valkey-master.my-app.svc:6379
```

## CI: GitHub Actions example

```yaml
# .github/workflows/deploy.yml
name: deploy
on:
  push:
    branches: [main]

permissions:
  id-token: write   # for OIDC auth to AWS
  contents: read

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ secrets.AWS_ROLE_ARN }}
          aws-region: us-east-1

      - name: Install bonsai
        run: |
          curl -sSL https://github.com/badriram/bonsai/releases/latest/download/bonsai-Linux-x86_64 \
            -o /usr/local/bin/bonsai
          chmod +x /usr/local/bin/bonsai

      - name: Provision / reconcile platform
        run: bonsai grow --provider aws --name my-app --env prod

      - name: Fetch kubeconfig
        run: |
          aws ssm get-parameter \
            --name /bonsai/my-app/prod/kubeconfig \
            --with-decryption \
            --query Parameter.Value --output text > ~/.kube/config

      - name: Deploy app
        run: kubectl apply -f k8s/
```

That's the entire integration. No infrastructure code in your app repo.

## CI: GitLab / generic example

```sh
# Same idea, any CI
bonsai grow --provider aws --name my-app --env prod

aws ssm get-parameter \
  --name /bonsai/my-app/prod/kubeconfig \
  --with-decryption \
  --query Parameter.Value --output text > ~/.kube/config

kubectl apply -f k8s/
```

## Reading secrets from app code

Postgres and Valkey URLs are also exposed inside the cluster as standard
secrets in your app namespace:

```yaml
# In your Deployment
env:
  - name: DATABASE_URL
    valueFrom:
      secretKeyRef: { name: bonsai-postgres, key: url }
  - name: REDIS_URL
    valueFrom:
      secretKeyRef: { name: bonsai-kv, key: url }
```

No need to plumb anything from Parameter Store into your pods — Bonsai mirrors
the URLs into the namespace as secrets during `grow`.

## `bonsai status`

```sh
$ bonsai status --name my-app --env prod
cluster: my-app/prod
healthy: true
workers: 3
k3s:     v1.31.0+k3s1
AMI:     ami-0abc123  (bonsai-node:2026-05-12)
```

## `bonsai logs`

Phase 1 stub. Phase 2 will tail any workload by name. For now use `kubectl
logs` once you have the kubeconfig.

## What you DON'T do

These are the operator's job, on a schedule. Not yours.

- Patch the OS
- Update k3s
- Rotate AMIs
- Upgrade CloudNativePG / Valkey
- Take Postgres backups (CNPG does this to S3 automatically)
