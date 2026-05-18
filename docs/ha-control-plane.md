# HA control plane (Phase 3)

Opt-in 3-node embedded-etcd control plane across multiple AZs, fronted by
a Network Load Balancer. Survives instance failure and AZ failure. Adds
~$60/month vs the single-node default (NLB ~$16, 2 extra t3.small ~$30,
multi-AZ data transfer ~$10–15).

## When to use it

Default single-node (Phase 1) is right for most apps:
- Single-AZ failure → 4–6 min API downtime, workloads keep running on
  workers, `rotate-control` brings it back from S3 snapshot
- ~$33/month total

HA is right when:
- You can't tolerate an AZ failure dropping the API
- You need rolling control-plane upgrades with zero API downtime
- You're running enough workloads that the extra $60/month is dwarfed
  by the cost of an outage

## Enable it

```sh
bonsai grow --provider aws --name my-app --env prod --ha-control --workers 3
```

The flag is sticky — once a cluster is provisioned HA, subsequent
`grow` calls keep it HA.

## Architecture

```
                  ┌────────────────────────────┐
                  │  Network Load Balancer     │
                  │  bonsai-<name>-<env>       │
                  │  TCP 6443                  │
                  └──────────┬─────────────────┘
                             │
            ┌────────────────┼────────────────┐
            ▼                ▼                ▼
       ┌────────┐       ┌────────┐       ┌────────┐
       │ ctrl-1 │       │ ctrl-2 │       │ ctrl-3 │
       │ AZ-a   │       │ AZ-b   │       │ AZ-c   │
       │ etcd ──┼───────┼─ etcd ─┼───────┼─ etcd  │  (embedded, raft quorum)
       └────────┘       └────────┘       └────────┘
                             │
                             ▼  workers
                        ┌──────────┐
                        │ ASG      │ (spread across all 3 subnets)
                        │ workers  │
                        └──────────┘
```

Workers' `CONTROL_PLANE_URL` is the NLB DNS — they don't care which
control plane node answers. `kubectl` from the operator works the same way.

## Phase 3 ship plan

| Part | Status | Scope |
|---|---|---|
| Part 1 | ✅ this PR | Multi-AZ subnets, NLB + TargetGroup + Listener infrastructure, `--ha-control` flag plumbed. Control plane still single-instance EIP — NLB exists with no targets. |
| Part 2 | ⏳ next PR | Control plane ASG (size 3, multi-AZ), `server.sh.tmpl` with SSM-lock-based etcd cluster-init coordination, worker user-data points at NLB DNS, `rotate-control` for HA becomes rolling ASG instance refresh. |
| Part 3 | ⏳ later | Hetzner equivalent — 3 Servers across regions/locations behind a Hetzner Load Balancer. |

## Why split this way

Part 1 is pure infrastructure scaffolding — multi-AZ subnets + NLB + TG +
Listener. Cheap to review in isolation. Worth checking the NLB design
(internet-facing? cross-zone load balancing? health check protocol?)
before Part 2 wires the control plane behind it.

Part 2 is the value: cluster-init coordination, ASG, etcd quorum,
worker reconfiguration. That gets its own focused review.

## SSM-lock-based cluster init (Part 2 design)

3 control plane EC2s come up simultaneously from the ASG. One must run
`k3s server --cluster-init` to initialize etcd; the other two must join
with `--server https://<nlb>:6443 --token …`.

The lock:

```sh
# In server.sh, before the k3s install:
if curl -k https://${NLB_DNS}:6443/healthz >/dev/null 2>&1; then
  # Cluster already exists — join
  install_join_existing
elif aws ssm put-parameter --name /bonsai/${NAME}/${ENV}/etcd-init-leader \
                            --type String --value ${INSTANCE_ID} \
                            --no-overwrite 2>/dev/null; then
  # Won the race — initialize
  install_cluster_init
else
  # Lost the race — wait for the leader to bring up etcd, then join
  wait_for_nlb_healthy
  install_join_existing
fi
```

The lock survives the cluster's lifetime (cheap; just one SSM parameter)
and prevents future replacement instances from accidentally
re-initializing.

## Network model

Same posture as Phase 1: public subnets only, no NAT. Multi-AZ adds:
- One /24 per AZ (10.0.1.0/24, 10.0.2.0/24, 10.0.3.0/24)
- Single route table associated with all subnets (default → IGW)
- Single VPC, single IGW

The NLB is internet-facing — workers and operators connect via its public
DNS. An internal NLB would force traffic through expensive cross-AZ
hops or VPC peering for operator access.

## Cost detail

| Component | Single-node | HA |
|---|---|---|
| Control plane EC2 | 1 × t3.small = $15 | 3 × t3.small = $45 |
| Workers | 1 × t3.small = $15 | 1 × t3.small = $15 |
| Elastic IP / NLB | EIP free while attached | NLB $16 + $0.006/LCU-hour |
| Cross-AZ data transfer | $0 | ~$5–10/month at typical load |
| Storage | ~$1 | ~$3 (3× EBS) |
| **Total** | **~$33/mo** | **~$90/mo** |
