# Operator guide

The operator sets Bonsai up once and runs scheduled maintenance. App
developers (see [`usage.md`](usage.md)) only see `grow`/`status`/`logs`.

Operator verbs are hidden from `bonsai --help` until you pass `--advanced`.

## One-time AWS setup

### IAM role for CI

```hcl
# Trust policy: GitHub OIDC -> bonsai role
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::ACCOUNT:oidc-provider/token.actions.githubusercontent.com" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
      },
      "StringLike": {
        "token.actions.githubusercontent.com:sub": "repo:YOUR_ORG/YOUR_APP:*"
      }
    }
  }]
}
```

Permissions the role needs:

- `ec2:*` scoped to resources tagged `bonsai:managed=true`
- `autoscaling:*` same scope
- `iam:CreateRole`, `iam:PutRolePolicy`, `iam:PassRole` (for instance profiles)
- `ssm:PutParameter`, `ssm:GetParameter` under `/bonsai/*`
- `s3:*` on the backup bucket

A reference policy ships in `docs/policies/bonsai-ci.json` (Phase 1.5).

### S3 backup bucket

```sh
aws s3 mb s3://bonsai-backups-ACCOUNTID --region us-east-1
```

CloudNativePG writes WAL + base backups here. One bucket per AWS account is
enough; backups are namespaced by `<name>/<env>/`.

## Updates — what's automatic, what isn't

| What         | How                                                       | Trigger        |
|--------------|-----------------------------------------------------------|----------------|
| Alpine pkgs  | `apk upgrade` systemd timer + `kured` for safe reboots    | Nightly        |
| k3s          | `system-upgrade-controller` applies Plan CRD              | Manual / cron  |
| AMI rebake   | `bonsai bake-ami` builds new image, tags `bonsai-node:latest` | Weekly cron |
| Worker roll  | `bonsai rotate-workers` ASG instance refresh              | Weekly cron    |
| Control roll | `bonsai rotate-control` snapshot + replace                | Manual         |
| CNPG/Valkey  | `bonsai upgrade --component <x>`                          | Manual         |

### Scheduled maintenance workflow

Lives in `.github/workflows/maintenance.yml` in **the Bonsai repo** (not
consuming app repos). Runs Mondays 03:00 UTC. Bakes a new AMI, rolls workers
on every registered cluster.

To register a cluster for auto-rotation, add it to `clusters.yaml`:

```yaml
clusters:
  - name: my-app
    env: prod
    region: us-east-1
  - name: my-app
    env: staging
    region: us-east-1
```

## Operator commands

```sh
bonsai bake-ami --advanced
```

Spins up a temporary Alpine builder, installs k3s + containerd + the standard
hardening config, snapshots to a new AMI, tags it as `bonsai-node:latest` and
`bonsai-node:<date>-<k3s-version>`. Previous five AMIs are kept; older ones
deregistered.

```sh
bonsai rotate-workers --advanced --name my-app --env prod --ami latest
```

Updates the ASG's launch template to the new AMI, triggers an instance
refresh, terminates old instances one at a time with a drain lifecycle hook.

```sh
bonsai rotate-control --advanced --name my-app --env prod
```

Phase 1 single-node control plane:

1. Snapshot etcd / SQLite to S3.
2. Terminate the current control plane instance.
3. Boot a new one from the latest AMI.
4. Restore state from the snapshot on first boot.
5. Wait for API to be healthy.

Workloads keep running on workers during this — only new pod scheduling is
paused. Plan a ~5 minute window.

```sh
bonsai upgrade --advanced --k3s v1.31.0+k3s1 --name my-app --env prod
```

Applies a `system-upgrade-controller` Plan CRD. The controller does the
draining and binary swap.

```sh
bonsai upgrade --advanced --component cnpg --name my-app --env prod
```

`helm upgrade` for the named component (cnpg | valkey | kured |
system-upgrade-controller).

```sh
bonsai destroy --advanced --name my-app --env prod
```

Irreversible. Tears down EC2, ASG, VPC, IAM, Parameter Store entries. S3
backups are kept (separate bucket lifecycle policy).

## Troubleshooting

- **`bonsai grow` hangs at "waiting for k3s API"** → check the control plane
  EC2 user-data logs: `aws ssm start-session ... && journalctl -u k3s`
- **Worker won't join** → token mismatch. Re-run `bonsai grow` to repair, or
  inspect `/bonsai/<name>/<env>/token` in Parameter Store.
- **CNPG cluster stuck `Initializing`** → check operator pod logs in the
  `cnpg-system` namespace; usually missing S3 credentials.
