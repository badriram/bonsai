#!/bin/sh
# k3s worker bootstrap. Runs on every ASG instance at first boot.
# Templated with: NAME, ENV, REGION, CONTROL_PLANE_URL.
set -eu

NAME="${NAME:?missing NAME}"
ENV="${ENV:?missing ENV}"
REGION="${REGION:?missing REGION}"
CONTROL_PLANE_URL="${CONTROL_PLANE_URL:?missing CONTROL_PLANE_URL}"

TOKEN="$(aws ssm get-parameter \
  --region "$REGION" \
  --name "/bonsai/${NAME}/${ENV}/token" \
  --with-decryption \
  --query Parameter.Value \
  --output text)"

curl -sfL https://get.k3s.io | \
  K3S_URL="$CONTROL_PLANE_URL" \
  K3S_TOKEN="$TOKEN" \
  sh -
