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
| libvirt | none — local hypervisor | see [Libvirt prerequisites](#libvirt-prerequisites) below |

### Libvirt prerequisites

Bonsai talks to libvirt via system-mode `qemu:///system`. That gives us a
real `default` NAT network, multi-VM addressability without port-forwarding
gymnastics, and the same code path on Linux and macOS.

**Linux** (Ubuntu/Debian/Fedora):

```sh
sudo apt-get install -y libvirt-daemon-system qemu-system-x86 qemu-utils genisoimage ovmf
sudo systemctl enable --now libvirtd
sudo virsh net-autostart default
sudo virsh net-start default
sudo usermod -aG libvirt $USER   # log out + back in so virsh runs without sudo
```

**macOS** (one-time, requires `sudo` for the daemon):

```sh
brew install libvirt qemu xorriso        # xorriso provides mkisofs
sudo brew services start libvirt          # libvirtd runs as root via launchd
sudo /opt/homebrew/sbin/virtlogd --daemon # libvirtd's log daemon (no brew service for it)
```

Brew's libvirt socket is root-only by default — you'll need sudo for every
`virsh` invocation, and `sudo bonsai grow` would leave kubeconfig + SSH
keys owned by root. Fix the socket once so your user can use it:

```sh
sudo sed -i.bak -E \
  -e 's|^#unix_sock_group = "libvirt"|unix_sock_group = "staff"|' \
  -e 's|^#unix_sock_rw_perms = "0770"|unix_sock_rw_perms = "0770"|' \
  -e 's|^#auth_unix_rw = "none"|auth_unix_rw = "none"|' \
  /opt/homebrew/etc/libvirt/libvirtd.conf
sudo brew services restart libvirt
```

macOS's `if_bridge` doesn't support the Linux-bridge rename libvirt's
`default` network needs, so Bonsai uses `<interface type='user'><source
mode='shared'/>` instead — qemu's vmnet-shared backend, giving each VM a
DHCP-leased 192.168.64.x address. No `virsh net-start default` needed.

`bonsai grow --provider libvirt` will refuse to start if the `default`
network isn't active and prints the exact `virsh net-start` command to run.

Apple Silicon hosts auto-pick the aarch64 Alpine cloud image. Override the
arch image with `BONSAI_LIBVIRT_IMAGE_URL` for bake-image outputs. macOS
hosts also default the domain accelerator to `qemu` (TCG) — set
`BONSAI_LIBVIRT_DOMAIN_TYPE=hvf` on libvirt >= 7.10 to get HVF acceleration.

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

## Phase 2.6: HA + tailnet (BYO headscale/tailscale)

Skip if not testing tailnet mode. Operator-supplied tailnet replaces the
NLB and admin-CIDR machinery entirely.

### Prerequisites

- A headscale instance OR Tailscale Cloud account.
- A device tag defined in your tailnet ACL (e.g. `tag:bonsai`) — operators
  pre-approved to use it.
- **Recommended:** an OAuth client scoped to `auth_keys:write` with tag
  `tag:bonsai`. Stored as SecureString:
  ```sh
  # in Tailscale admin: Settings → OAuth clients → Generate OAuth client
  # capability: Devices > Keys > Write, tags: tag:bonsai
  aws ssm put-parameter --name /myorg/secrets/tailnet-key \
    --type SecureString --value 'tskey-client-...'
  ```
  Each node mints its own one-shot ephemeral key on boot and is auto-pruned
  from the tailnet when the instance dies. No key rotation needed.

  **Or** a reusable pre-auth key (`tskey-auth-...`) — works the same in user-data
  but you'll need to rotate every 90 days and prune dead nodes manually.

### T1. `grow --ha-control` with tailnet flags

```sh
bonsai grow --provider aws --name $NAME --env $ENV --workers 2 \
  --ha-control \
  --tailnet-url=https://headscale.example.com \
  --tailnet-key-ssm=/myorg/secrets/tailnet-key \
  --tailnet-tag=tag:bonsai
```

**What's different from H1:**
- No NLB provisioned (~$53/mo saved).
- No `BONSAI_ADMIN_CIDR` SG rules — public 6443 is closed.
- Each control plane + worker installs tailscale on boot and joins your
  tailnet. The leader publishes its tailnet IP to
  `/bonsai/$NAME/$ENV/control-tailnet-ip`; joiners + workers read it.
- Kubeconfig server URL is the leader's tailnet IP.

### T2. kubectl from anywhere on the tailnet

```sh
aws ssm get-parameter --name /bonsai/$NAME/$ENV/kubeconfig --with-decryption \
  --query Parameter.Value --output text > /tmp/kc-tn
KUBECONFIG=/tmp/kc-tn kubectl get nodes
```

Your laptop just needs to be on the tailnet (`tailscale up` on the same
network). No SG holes, no IP staleness, no NLB.

### T3. Verify in your tailnet UI

Headscale: `headscale nodes list` — should show 3 `bonsai-$NAME-$ENV-*`
control plane entries + 2 worker entries.

### T4. Destroy

`bonsai destroy --advanced` works the same — terminates the ASG and ALL
the rest.

- **With OAuth client + ephemeral keys** (recommended): nodes go offline
  on terminate; Tailscale auto-removes them from the tailnet ~5 min later.
  Nothing to clean up.
- **With pre-auth keys**: instances die without `tailscale logout`, so
  ghost machines will linger in headscale until you prune them
  (`headscale nodes expire $node_id`).

## Phase 2.7: Hetzner HA (LB mode)

Skip if not testing Hetzner HA. Requires `HCLOUD_TOKEN` env var with project
admin scope.

### HZ1. `grow --ha-control` on Hetzner

```sh
export HCLOUD_TOKEN=...
export BONSAI_ADMIN_CIDR=$(curl -s -4 ifconfig.me)/32     # for SSH + LB-fronted 6443
bonsai grow --provider hetzner --name $NAME --env $ENV --workers 2 --ha-control
```

**What gets created:**
- Hetzner Network `bonsai-$NAME-$ENV` (10.0.0.0/16, subnets per location in eu-central)
- Hetzner Cloud Firewall `bonsai-$NAME-$ENV` (intra-network etcd/kubelet/flannel + admin 22/6443)
- Hetzner Load Balancer `bonsai-$NAME-$ENV` (lb11, ~€5/mo) on TCP:6443
- 3 control plane Servers (`cx22`) spread across `nbg1`, `fsn1`, `hel1`, all attached to the network + firewall
- 2 worker Servers in the network

**Expected timing:** 14–18 min (3 control planes sequenced — leader first, joiners after).

### HZ2. Verify 3 etcd + 2 workers Ready

```sh
KUBECONFIG=~/.bonsai/$NAME-$ENV/kubeconfig kubectl get nodes -o wide
```

Should show 3 `control-plane,etcd` + 2 `<none>` (workers), all Ready.

### HZ3. Confirm LB targets healthy

```sh
hcloud load-balancer describe bonsai-$NAME-$ENV
```

Look for `Targets:` block listing all 3 control planes as `healthy`.

### HZ4. `rotate-control --advanced` — one-at-a-time replacement

```sh
bonsai rotate-control --advanced --provider hetzner --name $NAME --env $ENV
```

Each iteration: delete a server → wait 60s for etcd member cleanup → create
replacement at same location → wait for k3s ready → reattach to LB. ~10 min
total for the 3 nodes. kubectl never errors (2 of 3 stay up).

### HZ5. Destroy

```sh
bonsai destroy --advanced --provider hetzner --name $NAME --env $ENV
```

Should sequence: LB → servers → floating IP → firewall → network → SSH key →
local secret dir. `hcloud server list -l bonsai:cluster=$NAME` returns empty.

## Phase 2.8: Hetzner HA + tailnet (BYO)

Same prereqs as Phase 2.6 (Tailscale OAuth client + `tag:bonsai` in ACL), but
the credential lives in a local file Bonsai reads at grow time (no SSM
equivalent on Hetzner).

```sh
echo 'tskey-client-...' > ~/.bonsai/_secrets/tailnet-key
chmod 600 ~/.bonsai/_secrets/tailnet-key

bonsai grow --provider hetzner --name $NAME --env $ENV --workers 2 \
  --ha-control \
  --tailnet-url=https://controlplane.tailscale.com \
  --tailnet-key-file=~/.bonsai/_secrets/tailnet-key \
  --tailnet-tag=tag:bonsai
```

**What's different from HZ1:**
- No Load Balancer provisioned (~€5/mo saved)
- No public 6443 on cluster servers — only members of your tailnet reach the API
- 5 nodes show up in your Tailscale admin under `tag:bonsai`

Verification, rotate-control, and destroy follow the same H2–H5 shape as the
AWS-tailnet sections (Phase 2.6) but reading kubeconfig from
`~/.bonsai/$NAME-$ENV/kubeconfig` instead of SSM.

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
