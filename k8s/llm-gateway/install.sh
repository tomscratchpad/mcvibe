#!/usr/bin/env bash
# Deploy LiteLLM + Postgres into llm-gateway.
# Generates the secrets on first run (never stored in git); re-runs keep them.
# Prereqs: ../policies applied (namespace + NetworkPolicies), ../egress-proxy deployed.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
NS=llm-gateway
rand() { openssl rand -hex 32; }

if ! kubectl -n $NS get secret postgres >/dev/null 2>&1; then
  kubectl -n $NS create secret generic postgres --from-literal=password="$(rand)"
fi
if ! kubectl -n $NS get secret litellm >/dev/null 2>&1; then
  pw="$(kubectl -n $NS get secret postgres -o jsonpath='{.data.password}' | base64 -d)"
  kubectl -n $NS create secret generic litellm \
    --from-literal=master-key="sk-$(rand)" \
    --from-literal=salt-key="sk-$(rand)" \
    --from-literal=database-url="postgresql://litellm:${pw}@postgres.${NS}.svc.cluster.local:5432/litellm"
fi

kubectl apply -k "$HERE"
kubectl -n $NS rollout status statefulset/postgres --timeout=300s
kubectl -n $NS rollout status deployment/litellm --timeout=600s
