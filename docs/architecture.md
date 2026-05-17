# Architecture

## What Bonsai is

A self-service infrastructure CLI. One command provisions a k3s cluster with
Postgres and KV included, for ~$35/month on AWS.

```
┌──────────────┐         ┌──────────────────┐
│  bonsai CLI  │────────▶│ Provider iface   │
└──────────────┘         └────────┬─────────┘
                          ┌───────┴────────┐
                          ▼                ▼
                     ┌─────────┐      ┌─────────┐
                     │   AWS   │      │ Hetzner │   (Phase 2)
                     │ aws-sdk │      │ hcloud  │
                     └────┬────┘      └────┬────┘
                          └────────┬───────┘
                                   ▼
                            ┌────────────┐
                            │ k3s cluster│
                            └─────┬──────┘
                                  │
              ┌───────────────────┼────────────────────┐
              ▼                   ▼                    ▼
       ┌───────────┐       ┌───────────┐       ┌──────────────┐
       │   CNPG    │       │   Valkey  │       │ sys-upgrade  │
       │ (Postgres)│       │   (KV)    │       │ + kured      │
       └───────────┘       └───────────┘       └──────────────┘
```

## Language: Go + aws-sdk-go-v2 direct

Not CDK. Not Pulumi. Not Terraform.

**Why Go:** single static binary. Drop into any CI without a runtime. Native
helm SDK, client-go, kustomize. Same ecosystem as k3s, kubectl, helm itself.

**Why direct SDK (no IaC framework):** the stack shape is fixed and small (VPC
+ EC2 + ASG + SSM + IAM). State = AWS resource tags + a small manifest in
Parameter Store. We query reality on every operation; no state file to wedge,
no CloudFormation stack to babysit, no Pulumi backend to host.

If we ever want declarative diff/destroy semantics, Pulumi Go can slot in
behind the same `PlatformProvider` interface — same language, no rewrite.

## Terminology: "workers", not "agents"

k3s calls non-server nodes "agents" internally; we keep that mapping in the
user-data. But the CLI surface and docs say **workers** to avoid (a) collision
with AI-agent terminology and (b) confusion with k8s's control-plane-vs-worker
mental model that app developers already understand.

## Network: public subnets only

```
VPC (10.0.0.0/16)
└── Public Subnet (us-east-1a)
    ├── EC2 control plane  ← public IP, IGW route
    └── ASG workers        ← public IP, IGW route

Internet Gateway (free)
```

No NAT Gateway. No VPC endpoints. No private subnets.

Rationale: every dependency the cluster talks to (Postgres-as-a-service if
not in-cluster, OIDC, AI APIs, S3) is on a public endpoint. NAT Gateway adds
~$32-98/month of fixed cost to route traffic that doesn't need it.

Security comes from security groups, not subnet privacy:

- `6443` (k3s API): admin CIDR + worker SG only
- `10250` (kubelet): intra-cluster SG only
- `22` (SSH): admin CIDR only, off by default
- egress: open

## Bootstrap flow

```
bonsai grow
   │
   ├─ ensureVPC           VPC + public subnets + IGW
   ├─ ensureIAM           instance profile w/ SSM + S3
   ├─ ensureK3sToken      random token → Parameter Store
   ├─ ensureControlPlane  EC2 + server user-data
   │     │
   │     └─ user-data: install k3s, publish token + kubeconfig to Parameter Store
   │
   ├─ ensureWorkers       ASG + worker user-data
   │     │
   │     └─ user-data: read token from Parameter Store, join cluster
   │
   ├─ cluster.Bootstrap   helm install:
   │     ├─ cert-manager
   │     ├─ CloudNativePG operator + Cluster CR
   │     ├─ Valkey
   │     ├─ system-upgrade-controller
   │     └─ kured
   │
   └─ writeOutputs        cluster_endpoint, kubeconfig, postgres_url, kv_url
                          → /bonsai/<name>/<env>/* in Parameter Store
                          + mirrored to k8s Secrets in app namespace
```

## Secret delivery: Parameter Store SecureString

Not AWS Secrets Manager.

| Service           | Cost                                  | Fit                  |
|-------------------|---------------------------------------|----------------------|
| Parameter Store   | Free (standard) + $0.05/10k API calls | Right tool           |
| Secrets Manager   | $0.40/secret/mo + API                 | Wasted on identity   |

Bonsai's outputs are cluster identity (kubeconfig, URLs, join token), not
rotation candidates. Parameter Store SecureString gives the same KMS
encryption story at a tenth of the cost.

The `secrets.Store` interface abstracts this — `ParameterStore`, `File`, and
(future) `Vault` implementations are interchangeable. Non-AWS providers
default to `File` writing to `~/.bonsai/<name>-<env>/`.

## Update model

| Layer              | Mechanism                              | Cadence    |
|--------------------|----------------------------------------|------------|
| Alpine packages    | `apk upgrade` timer + kured            | Nightly    |
| k3s                | `system-upgrade-controller` + Plan CRD | On bump    |
| Whole-node refresh | New AMI + ASG instance refresh         | Weekly     |
| CNPG / Valkey      | `helm upgrade`                         | On bump    |
| Bonsai CLI itself  | `bonsai self-update` (binary swap)     | On release |

The consuming app's CI never invokes any of these. They run on a schedule in
the Bonsai operator's repo, or manually via the `--advanced` commands.

## Provider interface

```go
type PlatformProvider interface {
    Provision(ctx context.Context, cfg ClusterConfig) (PlatformOutputs, error)
    Destroy(ctx context.Context, name, env string) error
    Status(ctx context.Context, name, env string) (PlatformStatus, error)
}

type PlatformOutputs struct {
    ClusterEndpoint    string
    KubeconfigLocation string
    PostgresURL        string
    KVURL              string
}
```

Same shape on every provider — that's the entire point. CLI never branches on
provider beyond constructing the right one.

## Phase plan

- **Phase 1** (in progress, 2 weeks) — AWS, single control plane, ASG
  workers, Tier 1 capabilities (Postgres + KV).
- **Phase 2** (1 week) — Hetzner provider via k3sup over SSH; proves the
  interface works.
- **Phase 3** (1 week) — HA 3-node control plane with embedded etcd, NLB.
- **Phase 4** (ongoing) — DigitalOcean, Tier 2 capabilities (object storage,
  observability addons), cost tagging.
