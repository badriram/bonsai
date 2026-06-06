# Bonsai

Self-service infrastructure CLI. One command provisions a k3s cluster on AWS
or Hetzner with Postgres and KV included, for ~$33–35/month at the small end.

## Install

**Homebrew** (macOS + Linux):

```sh
brew tap badriram/tools
brew install bonsai
# later:
brew upgrade bonsai
```

**Direct download** — grab the right tarball from
[Releases](https://github.com/badriram/bonsai/releases/latest) for your
OS/arch, extract, drop `bonsai` somewhere in `$PATH`.

**From source** (Go 1.26+):

```sh
git clone https://github.com/badriram/bonsai.git && cd bonsai
make build      # ./bonsai is the binary
```

## Local state — `BONSAI_DATA_DIR`

Bonsai keeps per-cluster operator state (kubeconfig, SSH keys for Hetzner,
secret store outputs) under `~/.bonsai/<name>-<env>/` by default. The kubeconfig
and SSH keys are sensitive — chmod 0600, but plaintext.

If you want them somewhere safer (encrypted volume, network share, secrets-managed
filesystem), set `BONSAI_DATA_DIR` to that path. Bonsai treats it as opaque
storage and stays out of how you secure it.

```sh
export BONSAI_DATA_DIR=/mnt/encrypted/bonsai          # LUKS, FileVault sparse bundle, gocryptfs, etc.
export BONSAI_DATA_DIR=~/Sync/bonsai                  # team-shared via end-to-end encrypted sync
```

For most single-operator setups, full-disk encryption + the default path is fine.

**Source-of-truth per provider:**

- **AWS**: SSM Parameter Store (SecureString) is the source of truth.
  `BONSAI_DATA_DIR` is a write-through cache — every read pulls from SSM and
  refreshes the local copy; every write hits SSM first and only updates the
  cache after. A sibling `<key>.meta.json` records the SSM path and the
  `refreshed_at` timestamp. If SSM is unreachable, commands fail rather than
  serving stale state. Cache layout matches the other providers
  (`<name>-<env>/<key>`), so apps and operators look in the same place
  regardless of provider — only the source of truth differs.
- **Hetzner / libvirt**: no managed remote secret store exists, so
  `BONSAI_DATA_DIR` is itself the source of truth. There is no team-shared
  recovery path baked in — if you need shared operator state, point
  `BONSAI_DATA_DIR` at an end-to-end encrypted sync target or a network share
  yourself.

## Per-cluster config — `bonsai.yaml`

Commit a `bonsai.yaml` next to your app to lock the operator intent in source
control. `bonsai grow` auto-discovers `./bonsai.yaml` (or pass `--config <path>`).
CLI flags still work and override individual fields for one-shot operations
(e.g. `bonsai grow --workers 5` to scale).

```yaml
name: my-app
env: prod
provider: hetzner
workers: 2
ha_control: true
admin_cidr: 198.51.100.42/32      # required unless tailnet.enabled
control_server_type: cpx22
worker_server_type: cpx22
locations: [nbg1, fsn1, hel1]
k3s_version: v1.31.0+k3s1
postgres:
  instances: 2
  volume_size: 10Gi      # per-instance PVC; bump for online resize on AWS/Hetzner
tailnet:
  enabled: true
  login_server: https://controlplane.tailscale.com
  tag: tag:bonsai
  auth_key_file: ~/.bonsai/_secrets/tailnet-key   # Hetzner
  # auth_key_ssm: /myorg/bonsai/tailnet-key       # AWS instead
```

Validation happens at load time — malformed credential files, missing
`admin_cidr`, typoed field names all fail before any cloud API call.
See [`bonsai.yaml.example`](bonsai.yaml.example) for a fully-documented template.

What's NOT in `bonsai.yaml`:

- **Secrets** (tokens, keys, kubeconfig) — live in `BONSAI_DATA_DIR/<name>-<env>/`
- **Inline `auth_key:`** — always use `auth_key_file` or `auth_key_ssm`, never paste a `tskey-*` into a committable file

## Quick start

### AWS (single-node, simplest)

```sh
export BONSAI_ADMIN_CIDR=$(curl -s -4 ifconfig.me)/32     # gates 6443/22 to your IP
bonsai grow --provider aws --name my-app --env prod --workers 2
```

### AWS with HA control plane (3 etcd nodes, multi-AZ)

```sh
bonsai grow --provider aws --name my-app --env prod --workers 2 \
  --ha-control                                              # ~$83/mo extra (NLB + 2 control plane nodes)
```

### AWS with HA + tailnet (recommended for HA — no public 6443)

Reuses an operator-supplied tailnet (Tailscale Cloud or self-hosted headscale).
Cluster API only reachable to members of your tailnet — no NLB, no admin-CIDR
holes. ~$53/mo NLB cost saved.

```sh
# one-time: create a tailnet OAuth client (Settings → OAuth clients) scoped to
# auth_keys:write + tag:bonsai; store the tskey-client-* secret in SSM:
aws ssm put-parameter --name /myorg/secrets/tailnet-key \
  --type SecureString --value 'tskey-client-...'

bonsai grow --provider aws --name my-app --env prod --workers 2 \
  --ha-control \
  --tailnet-url=https://controlplane.tailscale.com \
  --tailnet-key-ssm=/myorg/secrets/tailnet-key
```

Operator's `kubectl` reaches the cluster via tailnet IP — `tailscale up` on
your laptop, then `bonsai grow` and `kubectl get nodes` just work.

### Hetzner (single-node)

```sh
export HCLOUD_TOKEN=...
bonsai grow --provider hetzner --name my-app --env prod --workers 2
```

Cluster outputs:

| Field              | AWS                                  | Hetzner                                |
|--------------------|--------------------------------------|----------------------------------------|
| `CLUSTER_ENDPOINT` | public IP / NLB DNS / tailnet IP     | floating IP / tailnet IP               |
| `KUBECONFIG`       | `ssm:///bonsai/<name>/<env>/kubeconfig` | `file://<name>-<env>/kubeconfig`      |
| `POSTGRES_URL`     | in-cluster CloudNativePG             | same                                   |
| `KV_URL`           | in-cluster Valkey                    | same                                   |

## What you get

| | Description |
|---|---|
| **Compute** | k3s 1.31 on Graviton (`t4g.small`) by default on AWS; Ubuntu 24.04 on Hetzner (`cpx22`, x86 AMD — `cax11` arm stock is too thin to default to) |
| **Postgres** | CloudNativePG operator + a Postgres cluster you can connect to (S3-backed WAL on AWS). Per-instance PVC size set by `postgres.volume_size`; default 10Gi. Online expansion on AWS/Hetzner — bump the value and re-run `bonsai grow`. Libvirt is provisioned-upfront (qcow2 thin) and pinned at first grow; `bonsai plan` will WARN you. Shrinks are never applied. |
| **KV** | Valkey single-pod, in-cluster |
| **Cert mgmt** | cert-manager (cluster-issuer setup is yours) |
| **Reboots** | kured (rolling reboots on package updates) |
| **k3s upgrades** | system-upgrade-controller, driven by `bonsai upgrade --k3s vX.Y.Z` |
| **HA control plane** | optional `--ha-control` — 3 etcd nodes spread across AZs/locations |
| **Private connectivity** | optional `--tailnet-*` — bring your own tailscale/headscale; no public 6443 |

## CLI surface

Two audiences, two surfaces.

**Developer** (consumes Bonsai from app CI):

```
bonsai grow      # provision or reconcile
bonsai plan      # diff desired vs last-known vs cloud — no mutation
bonsai status    # what's running (state.json + live cloud)
```

`bonsai plan` exits 0 on no-changes, 2 on pending changes / drift, 1 on
error — drop-in for CI gating. Catches deprecated SKUs, missing
`admin_cidr`, and architecture-change toggles (e.g. tailnet on→off) before
any cloud-mutating call.

**Operator** (sets Bonsai up once, runs scheduled maintenance):

```
bonsai bake-image          # build new node image (AMI / Hetzner snapshot)
bonsai rotate-workers      # rolling worker replacement
bonsai rotate-control      # replace control plane node(s)
bonsai upgrade --k3s vX.Y  # apply system-upgrade-controller Plan
bonsai upgrade --component cnpg
bonsai destroy             # idempotent teardown (S3 backups preserved)
```

Operator verbs are hidden from `--help` unless `--advanced` is passed.

### `grow` flags worth knowing

| Flag | Purpose |
|---|---|
| `--provider aws \| hetzner` | which cloud |
| `--name`, `--env` | cluster identity; tagged on every resource |
| `--region` | cloud region (`us-east-1` for AWS, `nbg1`/`fsn1`/`hel1` for Hetzner) |
| `--workers N` | worker count (default 1) |
| `--ha-control` | 3-node embedded-etcd control plane across AZs/locations |
| `--tailnet-url` | headscale / tailscale login server (`https://controlplane.tailscale.com` for Tailscale Cloud) |
| `--tailnet-key-ssm` | SSM path to OAuth client secret (`tskey-client-*`) or pre-auth key (`tskey-auth-*`) — AWS only |
| `--tailnet-tag` | device tag advertised by nodes (default `tag:bonsai`; must be defined in your tailnet ACL) |

`BONSAI_ADMIN_CIDR=A.B.C.D/32` env var gates external access to ports 6443/22
on AWS (single-node or HA-NLB modes). Not needed in tailnet mode.

## Update model

- **k3s itself**: `bonsai upgrade --k3s vX.Y.Z` → system-upgrade-controller Plan
  CRDs roll the cluster node-by-node, controllers respect PDBs.
- **OS packages**: AL2023 / Ubuntu unattended-upgrades on the nodes; **kured**
  reboots them safely as needed.
- **Node images**: `bonsai bake-image` produces a hardened AMI (AWS) /
  snapshot (Hetzner) with k3s + container runtime baked in. `bonsai
  rotate-workers` + `bonsai rotate-control` roll the cluster onto the new image.
- **In-cluster operators** (cert-manager, CNPG, Valkey, kured, SUC):
  `bonsai upgrade --component <name>` via in-process Helm SDK against pinned
  versions in `internal/cluster/charts.go`. Bumps are intentional commits.

The consuming app's CI only ever runs `bonsai grow` + `kubectl apply`.
Everything else is autonomous or operator-driven.

## Documentation

- [`docs/smoke-test.md`](docs/smoke-test.md) — end-to-end manual test playbook
  for AWS, AWS+HA, AWS+HA+tailnet, and Hetzner
- [`ROADMAP.md`](ROADMAP.md) — what's shipped, what's next
- [`CLAUDE.md`](CLAUDE.md) — locked-in design decisions (don't re-litigate without a thread)

## Status

Current release: **v0.2.2** — structured `bonsai.yaml`, per-cluster `state.json`, and `bonsai plan` shipped. Both Hetzner HA modes (LB and tailnet-BYO) validated end-to-end on real Hetzner.

| Phase | State |
|---|---|
| Phase 1 — AWS single-node | shipped |
| Phase 2 — Hetzner provider | shipped |
| Phase 3 Part 1 — multi-AZ + NLB scaffolding (AWS) | shipped |
| Phase 3 Part 2 — HA control plane ASG + tailnet BYO (AWS) | shipped |
| Phase 3 Part 3 — Hetzner HA + LB + tailnet | shipped + smoke-validated |
| Phase 3 Part 4 — `bonsai.yaml` + `state.json` + `bonsai plan` | shipped |
| Phase 4 — libvirt provider | not started |

See [`ROADMAP.md`](ROADMAP.md) for the full picture.

## License

Apache 2.0. See [`LICENSE`](LICENSE).
