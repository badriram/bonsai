# Bonsai

Self-service infrastructure CLI. One command provisions a k3s cluster on AWS (later Hetzner, DO) with Postgres and KV included, for ~$35/month.

## Quick start

```sh
bonsai grow --provider aws --name my-app --env prod
```

Outputs (written to SSM under `/bonsai/<name>/<env>/`):

- `CLUSTER_ENDPOINT` — k3s API
- `KUBECONFIG`       — full kubeconfig
- `POSTGRES_URL`     — in-cluster CloudNativePG
- `KV_URL`           — in-cluster Valkey

## CLI surface

Two audiences, two surfaces.

**Developer** (consumes Bonsai from app CI):

```
bonsai grow      # provision or reconcile
bonsai status    # what's running
bonsai logs      # tail
```

**Operator** (sets Bonsai up once, runs scheduled maintenance):

```
bonsai bake-ami            # build new Alpine + k3s AMI
bonsai rotate-workers      # ASG instance refresh
bonsai rotate-control      # snapshot + replace control plane
bonsai upgrade --k3s vX.Y  # apply system-upgrade-controller Plan
bonsai upgrade --component cnpg
bonsai destroy
```

Operator verbs are hidden from `--help` unless `--advanced` is passed.

## Update model

- **k3s**: Rancher's `system-upgrade-controller`, installed during bootstrap.
- **OS (Alpine)**: nightly `apk upgrade` systemd timer + `kured` for safe reboots.
- **AMI baking**: weekly job in Bonsai's own GH Actions; `bonsai rotate-workers` rolls ASG.
- **Operators (CNPG, Valkey)**: `helm upgrade` via Go helm SDK.

The consuming app's CI only ever runs `bonsai grow` + `kubectl apply`. Updates are autonomous.

## Status

Phase 1 in progress — AWS provider, single control plane, ASG workers, Tier 1 capabilities.
