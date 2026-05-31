# Bonsai

Self-service infrastructure CLI. One command provisions a k3s cluster on AWS (later Hetzner, DO) with Postgres and KV included, for ~$35/month.

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
bonsai bake-image          # build new node image (AMI / snapshot)
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

Phase 1 (AWS) + Phase 2 (Hetzner) complete. Phase 3 (HA control plane on
AWS) in progress — Part 1 (multi-AZ + NLB scaffolding) shipped; Part 2
(3-node etcd ASG) next.

See [`ROADMAP.md`](ROADMAP.md) for the full plan and the next item to pick up.
