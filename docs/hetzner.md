# Hetzner provider

Phase 2. The same CLI surface as AWS, dispatched to Hetzner Cloud:

```sh
export HCLOUD_TOKEN=...
bonsai grow --provider hetzner --name my-app --env prod --region nbg1
```

## What's implemented

| Verb | Status |
|---|---|
| `grow` | ✅ |
| `destroy` | ✅ |
| `status` | ✅ |
| `upgrade --k3s` | ✅ |
| `upgrade --component` | ✅ |
| `bake-image` | ⏳ Phase 2.1 |
| `rotate-workers` | ⏳ Phase 2.1 |
| `rotate-control` | ⏳ Phase 2.1 |

## Differences from AWS

- **Compute**: Hetzner Cloud Servers (`cx22` default — 2 vCPU, 4GB RAM, ~€5/mo). No ASG primitive; Bonsai manages individual worker servers.
- **Stable endpoint**: a Floating IP (Hetzner's EIP equivalent) is assigned to the control plane. Survives rotation.
- **Bootstrap**: cloud-init runs the k3s install in the server user-data. Bonsai SSHs in afterward to read `/var/lib/rancher/k3s/server/node-token` and `/etc/rancher/k3s/k3s.yaml`.
- **Secret store**: `FileSecretStore` writes to `~/.bonsai/<name>-<env>/` (override with `BONSAI_DATA_DIR`). Hetzner has no managed parameter store; Object Storage is beta.
- **Backups**: `cluster.Bootstrap` runs CNPG without `barmanObjectStore` when `BackupBucket` is empty (the default for Hetzner). External S3 (Cloudflare R2, Backblaze B2, etc.) can be wired in as Phase 2.1.
- **No private network**: Phase 2 uses public IPs only with firewall rules — same posture as AWS Phase 1, no NAT.

## Region codes

Hetzner uses location codes instead of AWS region IDs. Pass via `--region`:

| Code | City |
|---|---|
| `nbg1` | Nuremberg, DE (default) |
| `fsn1` | Falkenstein, DE |
| `hel1` | Helsinki, FI |
| `ash`  | Ashburn, US-East |
| `hil`  | Hillsboro, US-West |

## What's stored locally

`~/.bonsai/<name>-<env>/`:
- `ssh_private_key` — Bonsai-generated, used to SSH the cluster's servers
- `token` — k3s join token
- `kubeconfig` — cluster API kubeconfig (with floating IP)
- `cluster_endpoint`
- `postgres_url`
- `kv_url`

Treat this directory like a credential. Back it up alongside your other operator state.

## Phase 2.1 plan

- `bake-image`: snapshot a stopped server via the Hetzner Image API, label as `bonsai-node:latest`.
- `rotate-workers`: delete + recreate worker servers one at a time, draining via the k8s API client first.
- `rotate-control`: SSH-snapshot of `/var/lib/rancher/k3s` → push to local store (or external S3) → delete control plane → recreate from latest image → restore-on-boot branch picks up state → reassign floating IP.
