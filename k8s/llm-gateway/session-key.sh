#!/usr/bin/env bash
# Issue / revoke the per-session LiteLLM virtual key.
#
#   session-key.sh issue  <session> <engineer>   # create key, install into the session pod
#   session-key.sh revoke <session>              # delete key (kill switch; also on session delete)
#   session-key.sh info   <session>              # spend / limits of the session key
#
# The virtual key is the ONLY credential the platform gives a session. It is
#  - scoped:    Claude models only, usable only from the agent-sessions namespace
#               (LiteLLM NetworkPolicy), so a stolen key is useless outside the cluster;
#  - bounded:   per-session $ budget, requests/min, tokens/min, parallel requests;
#  - expiring:  key lifetime = max session lifetime;
#  - attributed: user_id + session metadata on every spend log row.
# It is delivered over `kubectl exec` (stdin) into ~/.claude/settings.json and is
# never stored in a Kubernetes object (SandboxClaim env would be readable plaintext).
# Admin access = RBAC for port-forward in llm-gateway + the master key Secret.
set -euo pipefail
NS_GW=llm-gateway
NS_S=agent-sessions
# Per-session limits (API-list-price USD; for subscription traffic this is a usage meter).
SESSION_BUDGET_USD="${SESSION_BUDGET_USD:-20}"
SESSION_TTL="${SESSION_TTL:-8h}"
RPM="${RPM:-60}"
TPM="${TPM:-2000000}"
PARALLEL="${PARALLEL:-4}"
# Per-engineer limit across all their sessions, reset daily.
USER_BUDGET_USD="${USER_BUDGET_USD:-50}"

cmd="${1:?usage: $0 issue|revoke|info <session> [engineer]}"
session="${2:?session name}"
alias="session-${session}"

MASTER="$(kubectl -n $NS_GW get secret litellm -o jsonpath='{.data.master-key}' | base64 -d)"
PORT=$((20000 + RANDOM % 10000))
kubectl -n $NS_GW port-forward svc/litellm $PORT:4000 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$PORT/health/liveliness" && break; sleep 0.2; done
api() { curl -sS -H "Authorization: Bearer $MASTER" -H 'Content-Type: application/json' "http://127.0.0.1:$PORT$@"; }

case "$cmd" in
issue)
  engineer="${3:?engineer id}"
  kubectl -n $NS_S get pod "$session" >/dev/null   # session must exist
  # Engineer-level budget (idempotent: ignore "already exists").
  api /user/new -d "{\"user_id\":\"$engineer\",\"max_budget\":$USER_BUDGET_USD,\"budget_duration\":\"1d\"}" >/dev/null || true
  # One key per session: drop any previous one first.
  api /key/delete -d "{\"key_aliases\":[\"$alias\"]}" >/dev/null 2>&1 || true
  key="$(api /key/generate -d "{
      \"key_alias\": \"$alias\",
      \"user_id\": \"$engineer\",
      \"models\": [\"claude-*\", \"anthropic/*\"],
      \"max_budget\": $SESSION_BUDGET_USD,
      \"duration\": \"$SESSION_TTL\",
      \"rpm_limit\": $RPM,
      \"tpm_limit\": $TPM,
      \"max_parallel_requests\": $PARALLEL,
      \"metadata\": {\"session\": \"$session\", \"engineer\": \"$engineer\"}
    }" | python3 -c 'import json,sys; print(json.load(sys.stdin)["key"])')"
  # Install into the session (user-level Claude settings; managed settings still win).
  printf '{"env":{"ANTHROPIC_CUSTOM_HEADERS":"x-litellm-api-key: Bearer %s"}}\n' "$key" |
    kubectl -n $NS_S exec -i "$session" -- sh -c 'umask 077; mkdir -p ~/.claude && cat > ~/.claude/settings.json'
  echo "issued $alias for $engineer: budget \$$SESSION_BUDGET_USD, ttl $SESSION_TTL, rpm $RPM, tpm $TPM"
  ;;
revoke)
  api /key/delete -d "{\"key_aliases\":[\"$alias\"]}"; echo
  ;;
info)
  api "/key/list?key_alias=$alias&return_full_object=true" | python3 -c '
import json,sys
for k in json.load(sys.stdin).get("keys",[]):
    print({f:k.get(f) for f in ["key_alias","user_id","spend","max_budget","expires","rpm_limit","tpm_limit","max_parallel_requests","blocked"]})'
  ;;
*) echo "unknown command $cmd" >&2; exit 2 ;;
esac
