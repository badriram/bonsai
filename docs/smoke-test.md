# Smoke test playbook

End-to-end exercise of every Bonsai verb against a sandbox account. Run
this when a meaningful change lands — interface, provider impl, in-cluster
bootstrap — to catch breakage that unit-shaped checks (`make check`) can't.

**Budget:** ~1 hour per provider, ~$1–2 of cloud spend if you destroy at the end.

## What you need

| Provider | Credentials | One-time setup |
|---|---|---|
| AWS | `AWS_PROFILE` or `AWS_ACCESS_KEY_ID`+`AWS_SECRET_ACCESS_KEY`; default region | `aws s3 mb s3://bonsai-backups-<account-id>` (one-time per account) |
| Hetzner | `HCLOUD_TOKEN` env var | nothing — Bonsai allocates SSH keys + floating IPs itself |

Build a fresh binary first:

```sh
make build-slim
./bonsai --version
```

A test cluster name + env you'll use throughout:

```sh
export NAME=smoke
export ENV=test
```

## Phase 1: developer-surface happy path

Both providers go through the same script. Replace `<PROVIDER>` with `aws`
or `hetzner`.

### 1. `grow` — fresh provision

```sh
bonsai grow --provider <PROVIDER> --name $NAME --env $ENV --workers 2
```

**Watch:**
- AWS: CloudWatch logs / `aws ec2 describe-instances` for the control plane reaching `running`
- Hetzner: `hcloud server list` for both servers showing `running`
- Both: SSH (Hetzner) / SSM Session Manager (AWS) into the control plane and `journalctl -u k3s -f`

**Expected output (stdout):**
```
CLUSTER_ENDPOINT  https://<ip>:6443
KUBECONFIG        ssm:///bonsai/smoke/test/kubeconfig         # AWS
                  # or file://smoke-test/kubeconfig            # Hetzner
POSTGRES_URL      postgres://app:***@postgres-rw.smoke-test.svc:5432/app
KV_URL            redis://valkey-primary.smoke-test.svc.cluster.local:6379
```

**Expected timing:** 6–10 minutes (control plane: ~2 min, workers: ~2 min,
cluster bootstrap: ~4 min for cert-manager + CNPG + Valkey + kured + SUC).

### 2. Pull the kubeconfig and verify

```sh
# AWS
aws ssm get-parameter --name /bonsai/$NAME/$ENV/kubeconfig --with-decryption \
  --query Parameter.Value --output text > /tmp/kubeconfig
# Hetzner
cp ~/.bonsai/$NAME-$ENV/kubeconfig /tmp/kubeconfig

export KUBECONFIG=/tmp/kubeconfig
kubectl get nodes
kubectl get pods -A
```

**Expected:**
- 1 control-plane node + 2 workers, all `Ready`
- Pods `Running` in: `cert-manager`, `cnpg-system`, `kube-system` (kured + flannel + coredns), `system-upgrade`, and the app namespace `smoke-test` (Postgres cluster, Valkey)

### 3. `status`

```sh
bonsai status --provider <PROVIDER> --name $NAME --env $ENV
```

Expect `healthy: true`, `workers: 2`, an image ID, a k3s version
(or `unknown` on Hetzner Phase 2).

### 4. `grow` again — idempotency

```sh
bonsai grow --provider <PROVIDER> --name $NAME --env $ENV --workers 2
```

**Expected:** completes in ~30 seconds. No new resources created. Outputs
identical. This is the most important property of the developer surface.

### 5. App-side secret consumption

The cluster mirrors `POSTGRES_URL` and `KV_URL` into the app namespace as
k8s Secrets:

```sh
kubectl -n smoke-test get secret bonsai-postgres -o jsonpath='{.data.url}' | base64 -d
kubectl -n smoke-test get secret bonsai-kv -o jsonpath='{.data.url}' | base64 -d
```

**Expected:** the same URLs Bonsai printed. Apps consume via `secretKeyRef`
— they never need to touch AWS/Hetzner APIs.

### 6. Postgres connectivity smoke

```sh
kubectl -n smoke-test run pg-test --rm -it --image=postgres:16 --restart=Never -- \
  bash -c 'psql "$DATABASE_URL" -c "SELECT version();"' \
  --env DATABASE_URL=$(kubectl -n smoke-test get secret bonsai-postgres -o jsonpath='{.data.url}' | base64 -d)
```

**Expected:** PostgreSQL version string.

## Phase 2: operator-surface verbs

### 7. `bake-image` — produce a fresh node image

```sh
bonsai bake-image --advanced --provider <PROVIDER>
```

**Watch:**
- AWS: `aws ec2 describe-instances --filters Name=tag:bonsai:role,Values=image-builder` until terminated; `aws ec2 describe-images --owners self` for the new AMI
- Hetzner: `hcloud server list` shows the builder, then disappears; `hcloud image list -o columns=id,description,labels` for the new snapshot

**Expected timing:** 8–12 minutes (boot + k3s install + shutdown + snapshot).

**Expected:** new image tagged `bonsai-node:latest=true` (AWS) or
`bonsai-node.latest=true` (Hetzner). Previous holder loses the tag.
Snapshots beyond the most recent 5 are deregistered.

### 8. `rotate-workers` — roll workers onto the new image

```sh
bonsai rotate-workers --advanced --provider <PROVIDER> --name $NAME --env $ENV --image latest
```

**Watch:**
- `kubectl get nodes -w` — workers go `SchedulingDisabled` → disappear → reappear
- AWS: ASG instance refresh in the EC2 console
- Hetzner: `hcloud server list` shows worker servers being replaced one at a time

**Expected:** cluster never drops below `N-1` workers in `Ready`. Final
state: `N` `Ready` workers, all on the new image ID.

### 9. `upgrade --component` — refresh an in-cluster component

```sh
bonsai upgrade --advanced --provider <PROVIDER> --name $NAME --env $ENV --component valkey
```

**Watch:**
- `helm -n smoke-test history valkey` shows a new revision
- `kubectl -n smoke-test rollout status statefulset/valkey-primary`

**Expected:** new helm revision; Valkey pod recreated.

### 10. `upgrade --k3s` — apply a Plan CRD

Pick a k3s patch above what's currently running:

```sh
bonsai upgrade --advanced --provider <PROVIDER> --name $NAME --env $ENV --k3s v1.31.0+k3s1
kubectl get plans -n system-upgrade -w
```

**Expected:** `server-plan` then `agent-plan` reach `Complete`. Control
plane cordons + drains + binary swap + uncordons (visible in
`kubectl get nodes -w`). Workers follow after the server plan completes.

**Expected timing:** 5–10 min for control plane, +3 min per worker.

### 11. `rotate-control` — replace the control plane

```sh
bonsai rotate-control --advanced --provider <PROVIDER> --name $NAME --env $ENV
```

**Watch:**
- AWS: `aws ec2 describe-instances` — old control plane terminates, new one launches; `aws ec2 describe-addresses` shows the EIP re-associate
- Hetzner: `hcloud server list` shows the control server disappear and reappear; `hcloud floating-ip list` shows the FIP move
- Both: `kubectl get nodes` is unreachable for ~4–6 minutes (AWS) or ~5–10 min (Hetzner). When it returns, the cluster is intact — workers reconnect automatically.

**Expected:** the floating/elastic IP survives. Worker nodes show
`Ready` after the new control plane comes up. CNPG cluster reconciles
(may show `pg_isready` blips in logs during the API outage).

## Phase 2.5: HA control plane (AWS only)

Skip if you're not testing `--ha-control`. Runs after a clean destroy of the
single-node cluster (or in a fresh `NAME`/`ENV` to avoid the EIP→NLB
switchover edge case).

### H1. `grow --ha-control` — fresh HA provision

```sh
bonsai grow --provider aws --name $NAME --env $ENV --workers 2 --ha-control
```

**What's different from step 1:**
- VPC now has 3 subnets, one per AZ (`describe-subnets` confirms).
- Internet-facing NLB on TCP:6443 (`describe-load-balancers`).
- Control plane is a 3-instance ASG named `bonsai-$NAME-$ENV-control`, not
  a single EC2 with an EIP.
- `CLUSTER_ENDPOINT` is the NLB DNS, not a raw IP.

**Expected timing:** 10–14 minutes (3 control nodes need to elect + join via
SSM-lock before workers see a healthy API).

**Watch for cluster-init race:**
- One control instance wins `/bonsai/$NAME/$ENV/etcd-init-leader` —
  `aws ssm get-parameter --name /bonsai/$NAME/$ENV/etcd-init-leader`
  shows that instance ID. The other two should NOT have run `--cluster-init`
  (check `journalctl -u k3s` on each via SSM Session Manager — they should
  show `--server https://...:6443` in the args).

### H2. Cluster sees 3 control nodes

```sh
aws ssm get-parameter --name /bonsai/$NAME/$ENV/kubeconfig --with-decryption \
  --query Parameter.Value --output text > /tmp/kc-ha
KUBECONFIG=/tmp/kc-ha kubectl get nodes
```

**Expected:** 3 `control-plane,etcd,master` nodes + 2 workers, all `Ready`.

### H3. Kill a control node — API survives

```sh
# Terminate one control instance; ASG replaces it
aws ec2 describe-instances \
  --filters Name=tag:bonsai:cluster,Values=$NAME Name=tag:bonsai:role,Values=control-plane Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[0].InstanceId' --output text | head -1 | \
  xargs -I {} aws ec2 terminate-instances --instance-ids {}

# kubectl should keep responding throughout
while sleep 5; do KUBECONFIG=/tmp/kc-ha kubectl get nodes -o name; echo "---"; done
```

**Expected:** kubectl never errors out. The terminated node disappears from
`kubectl get nodes`, the ASG launches a replacement (~3 min), and it rejoins
as a fresh etcd member (the SSM-lock is held, so it takes the `--server`
join path).

### H4. `rotate-control` — rolling instance refresh, zero API downtime

```sh
bonsai bake-image --provider aws  # produce a fresh AMI
bonsai rotate-control --advanced --provider aws --name $NAME --env $ENV
```

**Expected:** triggers an ASG instance refresh with
`MinHealthyPercentage=67` so 2 of 3 stay up at all times. Watch:

```sh
aws autoscaling describe-instance-refreshes \
  --auto-scaling-group-name bonsai-$NAME-$ENV-control
```

API stays available the whole time (etcd quorum = 2 of 3).

### H5. Destroy

`bonsai destroy --advanced` already handles the HA path. Verify cleanup
also covers:
- `aws elbv2 describe-load-balancers` — no `bonsai-$NAME-$ENV` NLB
- `aws autoscaling describe-auto-scaling-groups` — no
  `bonsai-$NAME-$ENV-control` ASG
- `aws ec2 describe-launch-templates` — no `bonsai-$NAME-$ENV-control` LT
- All 3 subnets gone.

## Phase 3: teardown

### 12. `destroy`

```sh
bonsai destroy --advanced --provider <PROVIDER> --name $NAME --env $ENV
```

**Expected timing:** 4–8 minutes (instance termination is the slow part).

**Check no leaks:**
- AWS:
  - `aws ec2 describe-instances --filters Name=tag:bonsai:cluster,Values=$NAME Name=instance-state-name,Values=running,pending,stopping`
  - `aws ec2 describe-vpcs --filters Name=tag:bonsai:cluster,Values=$NAME`
  - `aws ec2 describe-addresses --filters Name=tag:bonsai:cluster,Values=$NAME`
  - `aws iam list-roles | grep bonsai-$NAME`
  - `aws ssm describe-parameters --parameter-filters Key=Name,Option=BeginsWith,Values=/bonsai/$NAME/`
  - All should return empty.
  - S3 backup bucket is intentionally preserved — clean manually if desired.
- Hetzner:
  - `hcloud server list -l bonsai.cluster=$NAME`
  - `hcloud floating-ip list -l bonsai.cluster=$NAME`
  - `hcloud ssh-key list -l bonsai.cluster=$NAME`
  - `ls ~/.bonsai/$NAME-$ENV/` — should not exist.

### 13. `destroy` again — idempotency

```sh
bonsai destroy --advanced --provider <PROVIDER> --name $NAME --env $ENV
```

**Expected:** completes cleanly in seconds. No errors despite nothing
to destroy.

## What to file as a bug

- Any step prints an error and aborts before the next step's preconditions
  are met (provisioning leaves orphan resources, destroy can't repeat).
- `grow` round-trip (step 1 → step 4) creates new resources instead of
  no-op'ing.
- App-namespace Secrets (`bonsai-postgres`, `bonsai-kv`) point at
  unreachable endpoints.
- Image rotation leaves the cluster below `N-1` workers `Ready` for more
  than 30 seconds.
- Control plane rotation doesn't restore the previous cluster state
  (visible as workers stuck `NotReady`, or kubectl rejecting the old CA).
- Destroy leaves resources tagged `bonsai:managed=true` behind in the cloud.

## Cost note

A complete run (provision → rotate → destroy) typically costs:

- AWS: ~$0.50 (mostly EBS + EIP-hours during the test window)
- Hetzner: ~€0.10 (server-hours billed per-hour; floating IP €1/month prorated)

If you forget to destroy, an idle cluster runs ~$33/month on AWS,
~€25/month on Hetzner. Set a reminder.
