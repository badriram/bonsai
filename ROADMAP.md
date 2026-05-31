# Roadmap

Living document. Pick the next item, work it on a branch, open a PR, update
this file when something ships. Full design context lives in the Banyan
trunk: https://banyan.vamitra.com/trunk/bnyn-ccb8a755 (public).

## Shipped

### Phase 1 — AWS core (complete)
Every verb working on a single-AZ EC2 + ASG topology, ~$33/mo.

| Verb | Description |
|---|---|
| `grow` | Provision/reconcile end-to-end: VPC → IAM → EIP → control plane → workers → cert-manager → CNPG → Postgres CR → Valkey → kured → SUC → outputs to Parameter Store + mirrored as k8s Secrets |
| `status` | Tag-based read of control plane + ASG state |
| `destroy` | Reverse-order idempotent teardown (S3 backups preserved) |
| `bake-image` | Builder EC2 → install k3s → CreateImage → tag `bonsai-node:latest` → prune beyond 5 |
| `rotate-workers` | Bump launch template AMI → ASG instance refresh, 50% min healthy |
| `rotate-control` | SSM Run Command snapshot → terminate → recreate → re-associate EIP → restore-on-boot in `server.sh` |
| `upgrade --k3s` | Apply SUC `server-plan` + `agent-plan` Plan CRDs |
| `upgrade --component` | helm upgrade against pinned versions in `cluster/charts.go` |

### Phase 2 — Hetzner provider (complete)
Same verb table, same interface. Mirrors: Tags→Labels, EC2+ASG→individual
Servers, EIP→Floating IP, Parameter Store→FileSecretStore at
`~/.bonsai/<name>-<env>/`, SSM Run Command→direct SSH. The PlatformProvider
interface required exactly **one** change to shared code:
`cluster.Bootstrap` makes CNPG `barmanObjectStore` conditional on
`BackupBucket != ""`.

### Phase 3 Part 1 — multi-AZ + NLB scaffolding (complete)
- Multi-AZ subnet support (`vpcInfra.SubnetIDs []string`, 3 across AZs when
  `--ha-control`)
- NLB + TargetGroup + Listener (`internal/provider/aws/nlb.go`),
  internet-facing TCP:6443
- `cli.grow --ha-control` flag, plumbed into Provision
- Control plane still single-instance EIP — Part 2 wires the ASG

### Phase 3 Part 2 — HA control plane ASG + etcd cluster-init (complete, PR #21)
- `server-ha.sh.tmpl` SSM-lock leader election: ASG launches 3 simultaneously,
  one wins `put-parameter --no-overwrite` and runs `--cluster-init`; the
  other two poll the NLB and join with `--server` + token
- `control_plane_ha.go` 3-node ASG spread across `net.SubnetIDs`, registered
  to the NLB TG via `TargetGroupARNs`
- `rotate-control` HA path = ASG `StartInstanceRefresh` (no snapshot
  needed — etcd quorum carries state); single-node snapshot+restore path preserved
- Latent bug fixes surfaced by end-to-end testing: IMDSv2 explicit token,
  etcd peer ports 2379-2380, VPC-CIDR allow for NLB target health checks,
  NLB TG `preserve_client_ip.enabled=false` to fix in-VPC asymmetric routing,
  flannel VXLAN as UDP not TCP, IAM instance-profile EC2/ASG propagation wait,
  helm `settings.Namespace` for charts that omit `metadata.namespace`,
  Bitnami valkey chart switched to OCI registry (HTTP repo returns 403 now)

### HA + tailnet mode (BYO headscale/tailscale, complete, PR #21)
- `--tailnet-url` + `--tailnet-key-ssm` + `--tailnet-tag`: nodes install
  tailscale on boot, join the operator's tailnet, k3s mesh runs over
  `tailscale0` (`--node-ip`, `--flannel-iface`, `--advertise-address`)
- OAuth client secret support (`tskey-client-*`): each node mints its own
  ephemeral tagged key, auto-pruned on instance death — no manual rotation,
  per-node blast radius. Pre-auth keys (`tskey-auth-*`) still work as
  fallback.
- Zero public 6443 surface in tailnet mode; no NLB; no admin-CIDR holes.
  Validated end-to-end: 3 control + 2 workers on tailnet, kubectl from
  operator laptop over tailnet, full in-cluster bootstrap healthy.

### Graviton/arm64 default (complete, PR #21)
`t3.small` → `t4g.small`, AL2023 AMI filter → `arm64`. ~20% per-instance
cost reduction; all bootstrap charts ship multi-arch.

### Admin-CIDR SG reconcile (complete, PR #22)
`bonsai grow` now tags admin rules with description `bonsai-admin` and
revokes any tagged rule whose CIDR no longer matches the current
`BONSAI_ADMIN_CIDR`. Previously the SG accumulated every IP we'd ever
used. Empty CIDR (e.g. switched to tailnet mode) revokes all.

### Release pipeline + Homebrew tap (complete)
- `.goreleaser.yaml` + `.github/workflows/release.yml` — tag-driven
  releases. Builds for `darwin/{amd64,arm64}` + `linux/{amd64,arm64}`,
  uploads tarballs + checksums to the GitHub release.
- Homebrew tap publish: every tag produces a new `Formula/bonsai.rb` in
  `badriram/homebrew-tools` (the same tap that holds other formulae like
`markdown-preview`). Users: `brew tap badriram/tools && brew install bonsai`.
- One-time operator setup before first release: create a PAT with `repo`
  scope on the existing `badriram/homebrew-tools` repo and add it as the
  `HOMEBREW_TAP_GITHUB_TOKEN` secret on this repo.

## In progress

Nothing. Pick from "Next up" below.

## Next up

### 1. Phase 3 Part 3 — Hetzner HA  **[do first]**

3 Servers across locations (`nbg1`, `fsn1`, `hel1`) behind a Hetzner Load
Balancer. Same SSM-lock pattern doesn't apply on Hetzner (no SSM); use a
sentinel file in the operator's `FileSecretStore` as the lock, written
before `Server.Create` for the leader. Or: Bonsai picks the leader
imperatively (first server in the loop), waits for etcd, then creates the
other two. Imperative leader-pick is honest for Hetzner since the provider
isn't ASG-driven anyway.

Tailnet-BYO mode on Hetzner is much simpler than on AWS — Hetzner already
has no SSM/IAM ceremony — but the same `--tailnet-url`/`--tailnet-key-ssm`
flag surface should work (token can live in `FileSecretStore` instead of
SSM).

### 2. External S3 backups for Hetzner (CNPG PITR)

Hetzner clusters currently run CNPG without `barmanObjectStore` because
Hetzner has no managed S3. Wire in support for external S3-compatible
endpoints (Cloudflare R2, Backblaze B2, Wasabi). Operator provides
credentials via env (e.g. `BONSAI_S3_ENDPOINT`, `BONSAI_S3_ACCESS_KEY`,
`BONSAI_S3_SECRET_KEY`); `cluster.Bootstrap` includes them in the CNPG
`s3Credentials` block.

Mostly mechanical once the env-vars and config plumbing exist.

## Phase 4 (from the Banyan trunk)

- **DigitalOcean provider** — third cloud, same interface. DO has Droplets
  (compute), Reserved IPs (EIP equivalent), Spaces (S3-compatible — CNPG
  backups work natively), and Load Balancers. Should be the cheapest
  provider to add since the patterns are now well-trodden.
- **GCP provider** — fourth cloud. More involved because GCP's IAM model is
  unique and managed instance groups behave differently from ASGs.
- **Tier 2 capabilities** — opt-in addons during `grow`:
  - Object storage (S3 on AWS, Spaces on DO, R2 on Hetzner-via-Cloudflare)
  - Background jobs (workqueue + Redis Streams)
  - Observability (Loki + Grafana + Prometheus operator)
- **Cost tagging per app + env** — surface in `bonsai status` and as a
  weekly report.

## Operational / quality debt

These don't unblock new features but pay back over time.

- **Tests.** Zero unit tests in the repo. At minimum, mock-based tests for
  the provider interfaces (verifying the lookup-by-tag → create-if-missing
  patterns). Also: a `cluster/charts.go` test that asserts pinned versions
  are reachable HTTP-wise so a typo in `RepoURL` fails locally before CI.
- **Binary size.** Currently 104MB. `-ldflags="-s -w" -trimpath` got us
  from 145MB → 99MB. UPX could take us to ~25–30MB at the cost of a
  release-build step + AV false-positive risk. Worth it once we have
  release artifacts to publish.
- **`bonsai logs` stub.** Drop a `kubectl logs` wrapper for the app
  namespace.
- **Status's `k3s_version`.** AWS path returns `"unknown"` because we never
  publish the running version to Parameter Store. Have `server.sh` write
  `k3s --version` to a parameter; Status reads it.
- **ASG-of-1 with restore-on-boot** for the single-node control plane —
  wrap today's single-instance control plane in a 1-instance ASG; on
  instance death, ASG launches a replacement and user-data restores from
  the latest S3 snapshot before starting k3s. Same code path
  `rotate-control` already uses. Cuts unplanned RTO from "manual
  intervention" to ~3-4 min, no extra cost.
- **External SQL datastore option** — k3s `--datastore-endpoint=postgres://...`
  to a managed DB (RDS, etc.). Cluster state survives instance death with
  near-zero RTO. Middle ground between single-node-with-snapshot and full
  `--ha-control`.

## How to resume

1. Read this file + `docs/ha-control-plane.md` + the Banyan trunk's
   Implementation Plan branch (`bnyn-c2edd5cc`).
2. Read `CLAUDE.md` for the locked-in decisions you should not re-litigate.
3. Pick the next item under **Next up**.
4. Run `bonsai checkin` (or `mcp__claude_ai_Banyan__banyan_checkin`) with
   `trunk_id=bnyn-ccb8a755` for the latest handoff.
5. Branch, work, PR, update this file.

## Process notes (don't repeat these mistakes)

- **Verify CI is green by actually running `gh pr checks <#>`.** I
  reported "CI green" several times based on reading the wrong PR's
  status; the entire branch was broken from PR #6 through #15 because
  `cmd/bonsai/main.go` was being silently ignored by an unanchored
  `.gitignore` rule. Local builds passed (file existed on disk), CI did
  not. Fix landed in PR #16. **Treat "build works locally" as half the
  evidence.**
- **Anchor every gitignore rule.** `bonsai` matches anywhere; `/bonsai`
  matches only the root.
- **Scope PRs to one reviewable concern.** Phase 3 was tempting to ship
  as one mega-PR; splitting into Part 1 (scaffolding) and Part 2 (control
  plane) means each is independently understandable.
- **Pin dependency versions in code, not floating.** SUC, CNPG, Valkey,
  kured, and k3s versions all live in `cluster/charts.go` or named
  constants in the provider packages. Bumps must be commits.
