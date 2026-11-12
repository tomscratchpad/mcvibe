#!/usr/bin/env bash
# Verify the LLM gateway + egress proxy controls from inside a real session.
#   A. Gateway auth : no key / forged key / out-of-scope model rejected;
#                     valid key without a Claude login fails closed (no fallback credential).
#   B. Spend limits : zero budget, rate limit and expired key enforced by LiteLLM.
#   C. Egress proxy : sign-in hosts allowed (incl. api.anthropic.com in subscription
#                     mode: accepted trade-off); other hosts, plain HTTP, IP literals,
#                     SNI not on the sessions allowlist, and no-proxy access denied.
#   D. Backend      : session cannot reach Postgres; proxy gateway listener not reachable from sessions.
# KEEP=1 keeps the test session.
set -uo pipefail
NS_S=agent-sessions; NS_GW=llm-gateway
HERE="$(cd "$(dirname "$0")" && pwd)"
pass=0; fail=0
ok()  { echo "  PASS  $*"; pass=$((pass+1)); }
bad() { echo "  FAIL  $*"; fail=$((fail+1)); }
expect() { # <grep -E pattern> <description> <output>
  if echo "$3" | grep -qE "$1"; then ok "$2"; else bad "$2: got: $(echo "$3" | head -c 200)"; fi; }

echo "== Setup: session 'gwtest' + per-session key"
kubectl -n $NS_S delete sandboxclaim gwtest --ignore-not-found --wait=true >/dev/null
kubectl -n $NS_S wait --for=delete pod/gwtest --timeout=120s >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata: { name: gwtest, namespace: $NS_S }
spec: { warmPoolRef: { name: claude-code }, lifecycle: { shutdownPolicy: Delete } }
EOF
kubectl -n $NS_S wait --for=condition=Ready sandboxclaim/gwtest --timeout=180s >/dev/null
"$HERE/../session-key.sh" issue gwtest verify-bot

# Admin side: short-lived keys for limit tests (port-forward + master key).
MASTER="$(kubectl -n $NS_GW get secret litellm -o jsonpath='{.data.master-key}' | base64 -d)"
PORT=$((20000 + RANDOM % 10000))
kubectl -n $NS_GW port-forward svc/litellm $PORT:4000 >/dev/null 2>&1 & PF=$!
trap 'kill $PF 2>/dev/null' EXIT
for _ in $(seq 50); do curl -s -o /dev/null "localhost:$PORT/health/liveliness" && break; sleep 0.2; done
admin() { curl -s -H "Authorization: Bearer $MASTER" -H content-type:application/json "localhost:$PORT$1" -d "$2"; }
gen() { admin /key/generate "$1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["key"])'; }
admin /key/delete '{"key_aliases":["t-budget0","t-rpm1","t-expired"]}' >/dev/null
K0=$(gen '{"key_alias":"t-budget0","models":["claude-*"],"max_budget":0}')
K1=$(gen '{"key_alias":"t-rpm1","models":["claude-*"],"rpm_limit":1}')
KX=$(gen '{"key_alias":"t-expired","models":["claude-*"],"duration":"1s"}'); sleep 2

# in_session <script>: run bash inside the session; $KEY = the session's own key
in_session() { kubectl -n $NS_S exec gwtest -- bash -c '
KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser(\"~/.claude/settings.json\")))[\"env\"][\"ANTHROPIC_CUSTOM_HEADERS\"].split()[-1])")
msg() { curl -s -m 30 -H "x-litellm-api-key: Bearer $1" -H content-type:application/json \
  -d "{\"model\":\"${2:-claude-haiku-5-5}\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
  $ANTHROPIC_BASE_URL/v1/messages; }
code() { curl -s -o /dev/null -m 10 -w "%{http_code}" "$@" || true; }
'"$1" 2>/dev/null; }

echo "== A. Gateway authentication"
expect '"code":"401"|auth_error'        "request without virtual key rejected"   "$(in_session 'curl -s -m 10 -H content-type:application/json -d "{}" $ANTHROPIC_BASE_URL/v1/messages')"
expect 'Invalid proxy server token'     "forged virtual key rejected"            "$(in_session 'msg sk-forged')"
expect 'key_model_access_denied'        "model outside key scope rejected"      "$(in_session 'msg $KEY gpt-5')"
expect 'Missing Anthropic API Key'      "valid key, no Claude login: fails closed (no fallback credential)" "$(in_session 'msg $KEY')"

echo "== B. Spend controls"
expect 'budget_exceeded'                "zero-budget key blocked"               "$(in_session "msg $K0")"
in_session "msg $K1" >/dev/null
expect 'rate_limit_error|Rate limit'    "2nd request within rpm=1 rejected"     "$(in_session "msg $K1")"
expect 'Expired Key'                    "expired key rejected"                  "$(in_session "msg $KX")"

echo "== C. Egress proxy (sessions listener)"
expect '^200$' "sign-in host platform.claude.com allowed"         "$(in_session 'code https://platform.claude.com/')"
expect '^[1-5][0-9][0-9]$' "api.anthropic.com reachable (subscription-mode sign-in; accepted trade-off, see README)" "$(in_session 'code https://api.anthropic.com/v1/models')"
expect '^000$' "api.openai.com denied (gateway-only host)"        "$(in_session 'code https://api.openai.com/')"
expect '^000$' "github.com denied"                                 "$(in_session 'code https://github.com/')"
expect '^000$' "registry.npmjs.org denied"                         "$(in_session 'code https://registry.npmjs.org/')"
expect '^403$' "plain HTTP denied"                                 "$(in_session 'code -x $HTTPS_PROXY http://claude.ai/')"
expect '^000$' "IP literal denied"                                 "$(in_session 'code -k https://160.79.104.10/')"
expect '^000$' "SNI mismatch denied (CONNECT claude.ai, TLS SNI api.openai.com)" \
  "$(in_session 'code --connect-to api.openai.com:443:claude.ai:443 https://api.openai.com/')"
expect '^000$' "direct internet without proxy denied"              "$(in_session 'code --noproxy "*" https://platform.claude.com/')"

echo "== D. Backend isolation"
expect '^000$' "session -> Postgres:5432 blocked"                  "$(in_session 'code --noproxy "*" telnet://postgres.llm-gateway.svc.cluster.local:5432')"
expect '^000$' "session -> proxy gateway listener :3129 blocked"   "$(in_session 'code -x http://egress-proxy.egress-proxy.svc.cluster.local:3129 https://api.anthropic.com/')"

admin /key/delete '{"key_aliases":["t-budget0","t-rpm1","t-expired"]}' >/dev/null
if [[ "${KEEP:-0}" != 1 ]]; then
  "$HERE/../session-key.sh" revoke gwtest >/dev/null
  kubectl -n $NS_S delete sandboxclaim gwtest --wait=false >/dev/null
fi
echo "== $pass passed, $fail failed"
[[ $fail -eq 0 ]]
