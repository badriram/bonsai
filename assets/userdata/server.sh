#!/bin/sh
# k3s server bootstrap. Runs once on the control plane EC2 at first boot.
# Templated by Bonsai with: NAME, ENV, REGION.
set -eu

NAME="${NAME:?missing NAME}"
ENV="${ENV:?missing ENV}"
REGION="${REGION:?missing REGION}"

# k3s installs itself with embedded SQLite (Phase 1). Phase 3 swaps in
# --cluster-init for embedded etcd HA.
curl -sfL https://get.k3s.io | sh -s - server \
  --write-kubeconfig-mode=0600 \
  --tls-san="$(hostname -f)" \
  --disable=traefik

# Publish the cluster join token to SSM so workers can find it.
TOKEN="$(cat /var/lib/rancher/k3s/server/node-token)"
aws ssm put-parameter \
  --region "$REGION" \
  --name "/bonsai/${NAME}/${ENV}/token" \
  --type SecureString \
  --value "$TOKEN" \
  --overwrite

# Publish the kubeconfig (server-ip rewritten to the public hostname).
KUBECFG="$(sed "s|127.0.0.1|$(curl -s http://169.254.169.254/latest/meta-data/public-hostname)|g" /etc/rancher/k3s/k3s.yaml)"
aws ssm put-parameter \
  --region "$REGION" \
  --name "/bonsai/${NAME}/${ENV}/kubeconfig" \
  --type SecureString \
  --value "$KUBECFG" \
  --overwrite
