#!/usr/bin/env bash
# Install Kata Containers on the cluster with the upstream kata-deploy chart.
# Needs: helm >= 3.x, kubeconfig with cluster-admin. Node needs /dev/kvm
# (nested virtualization enabled when the node is itself a VM).
set -euo pipefail
KATA_VERSION="${KATA_VERSION:-4.3.1}"
HERE="$(cd "$(dirname "$0")" && pwd)"

helm upgrade --install kata-deploy \
  oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version "${KATA_VERSION}" \
  --namespace kube-system \
  --values "${HERE}/values.yaml" \
  --wait --timeout 15m
