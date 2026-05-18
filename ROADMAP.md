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

## In progress

Nothing — Part 1 just merged. Resume from "Next up" below.

## Next up

### 1. Phase 3 Part 2 — HA control plane ASG + etcd cluster-init  **[do first]**

Wires the 3-node embedded-etcd control plane behind the NLB Part 1 set up.
The biggest open item.

**Design open** (resolve via Banyan thread or in the PR description before coding):

1. **NLB scheme — internet-facing vs internal.** Current Part 1 creates
   internet-facing. Recommendation: keep internet-facing, gate 6443 via
   security group + `BONSAI_ADMIN_CIDR`. Internal NLB forces a bastion/VPN
   for operator `kubectl`, which is friction for the small-team use case.

2. **Cluster-init coordination — SSM-lock vs sequential launch.**
   - **SSM-lock:** ASG launches all 3 simultaneously. Each runs
     `aws ssm put-parameter --name /bonsai/.../etcd-init-leader --no-overwrite`.
     The one that succeeds runs `k3s server --cluster-init`; the others
     poll `https://<nlb>:6443/healthz` and join with `--server` + token.
     Simple, idempotent on replacement, ~10s race window.
   - **Sequential launch:** Bonsai `RunInstances` the first node directly,
     waits for etcd quorum, then creates the ASG with size 2 to join.
     Cleaner ordering, more orchestration code, replacement still uses
     join (which is correct).
   - **Recommendation:** SSM-lock. The 10s window is harmless; the
     `--no-overwrite` is genuinely atomic; replacement just works.

3. **HA default vs opt-in for `--env prod`.** Current `--ha-control` is
   opt-in. Could auto-enable for `--env prod`. Adds ~$60/mo, which
   matters at the cost tier Bonsai exists for. **Recommendation:** stay
   opt-in. Cost is the headline; let the operator decide.

**Files this PR will touch:**
- `internal/provider/aws/userdata/server-ha.sh.tmpl` (new) — SSM-lock init/join
- `internal/provider/aws/userdata.go` — render the HA server template
- `internal/provider/aws/control_plane_ha.go` (new) — ASG-based 3-node
  control plane with TG registration via launch-template `TargetGroupARNs`
- `internal/provider/aws/provider.go` — Provision branches on `cfg.HAControl`:
  HA path replaces ensureControlEIP+ensureControlPlane+associateControlEIP
  with ensureControlPlaneASG (already-attached to TG); workers use NLB DNS
- `internal/provider/aws/workers.go` — `controlPlaneURL` becomes NLB DNS
  when HA
- `internal/provider/aws/rotate_control.go` — HA path = ASG instance refresh
  (no snapshot needed; etcd quorum carries state); preserve single-node
  snapshot+restore path
- `internal/provider/aws/destroy.go` — destroy control ASG (separate from
  worker ASG)

**Test plan:** Smoke test (`docs/smoke-test.md`) with `--ha-control` flag.
Verify: 3 control plane nodes show up in `kubectl get nodes`, one
initialized + two joined, rotate-control replaces them rolling without API
downtime, destroy cleans up.

### 2. Phase 3 Part 3 — Hetzner HA

3 Servers across locations (`nbg1`, `fsn1`, `hel1`) behind a Hetzner Load
Balancer. Same SSM-lock pattern doesn't apply on Hetzner (no SSM); use a
sentinel file in the operator's `FileSecretStore` as the lock, written
before `Server.Create` for the leader. Or: Bonsai picks the leader
imperatively (first server in the loop), waits for etcd, then creates the
other two. Imperative leader-pick is honest for Hetzner since the provider
isn't ASG-driven anyway.

### 3. External S3 backups for Hetzner (CNPG PITR)

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
- **Release pipeline.** GitHub Releases workflow that builds slim binaries
  for `darwin/{amd64,arm64}` + `linux/{amd64,arm64}` and publishes on tag.
- **Homebrew tap.** Once releases exist, publish `badriram/tap/bonsai`.

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
