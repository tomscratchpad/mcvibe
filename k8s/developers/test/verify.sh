#!/usr/bin/env bash
# Developer isolation tests: RBAC + owner admission policies.
#
# Runs with an admin kubeconfig and impersonates two onboarded developers
# (kubectl --as), so no IdP is needed. Prerequisites:
#   ../developer.sh add alice@corp.com && ../developer.sh add bob@corp.com
#   alice has a Ready sandbox (default "demo"):  sbx --as alice@corp.com create demo
#
#   test/verify.sh [sandbox-name]
set -uo pipefail
A=alice@corp.com B=bob@corp.com NA=sbx-alice NB=sbx-bob SBX=${1:-demo}
pass=0 fail=0
ok()  { echo "  PASS  $1"; pass=$((pass+1)); }
bad() { echo "  FAIL  $1"; echo "        $2" | head -3; fail=$((fail+1)); }

# expect <allow|deny> <description> <expected substring in denial or ""> -- cmd...
expect() {
  local want=$1 desc=$2 needle=$3; shift 4
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [[ $want == allow ]]; then
    [[ $rc -eq 0 ]] && ok "$desc" || bad "$desc" "$out"
  else
    if [[ $rc -ne 0 && $out == *"$needle"* ]]; then ok "$desc  [$needle]"; else bad "$desc (rc=$rc)" "$out"; fi
  fi
}

claim() { # claim <ns> <name> <owner-annotation>
  cat <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata: { name: $2, namespace: $1, annotations: { agent-platform/owner-username: "$3" } }
spec: { warmPoolRef: { name: claude-code } }
EOF
}

POD=$(kubectl -n $NA get pod "$SBX" -o name 2>/dev/null) || { echo "need a Ready sandbox $SBX in $NA"; exit 2; }
POD=${POD#pod/}

echo "== A. owner uses their own namespace"
expect allow "alice lists her claims"                     "" -- kubectl --as $A -n $NA get sandboxclaims
expect allow "alice execs into her sandbox"               "" -- kubectl --as $A -n $NA exec "$POD" -c agent -- true
expect allow "alice creates a claim (dry-run)"            "" -- sh -c "$(declare -f claim); claim $NA t1 $A | kubectl --as $A create --dry-run=server -f -"

echo "== B. RBAC: another developer is locked out"
expect deny "bob lists alice's claims"            "forbidden" -- kubectl --as $B -n $NA get sandboxclaims
expect deny "bob execs into alice's sandbox"      "forbidden" -- kubectl --as $B -n $NA exec "$POD" -c agent -- true
expect deny "bob deletes alice's claim"           "forbidden" -- kubectl --as $B -n $NA delete sandboxclaim "$SBX" --dry-run=server
expect deny "bob creates a claim in alice's ns"   "forbidden" -- sh -c "$(declare -f claim); claim $NA t2 $B | kubectl --as $B create --dry-run=server -f -"

echo "== C. RBAC: what alice may not do even in her own namespace"
expect deny "alice creates a bare Sandbox"        "" -- kubectl --as $A -n $NA auth can-i create sandboxes.agents.x-k8s.io --quiet
expect deny "alice creates a Pod"                 "" -- kubectl --as $A -n $NA auth can-i create pods --quiet
expect deny "alice port-forwards"                 "" -- kubectl --as $A -n $NA auth can-i create pods/portforward --quiet
expect deny "alice edits the template"            "" -- kubectl --as $A -n $NA auth can-i update sandboxtemplates.extensions.agents.x-k8s.io --quiet
expect deny "alice edits her namespace (owner)"   "" -- kubectl --as $A auth can-i update namespace/$NA --quiet
expect deny "alice reads secrets"                 "" -- kubectl --as $A -n $NA auth can-i get secrets --quiet
expect deny "alice execs in agent-sessions"       "" -- kubectl --as $A -n agent-sessions auth can-i create pods/exec --quiet
expect deny "alice execs in llm-gateway"          "" -- kubectl --as $A -n llm-gateway auth can-i create pods/exec --quiet

echo "== D. Admission: claim owner must be the caller"
expect deny "alice creates a claim owned by bob"  "must be your own username" -- sh -c "$(declare -f claim); claim $NA t3 $B | kubectl --as $A create --dry-run=server -f -"
expect deny "alice creates a claim with no owner" "must be your own username" -- sh -c "$(declare -f claim); claim $NA t4 '' | kubectl --as $A create --dry-run=server -f -"

echo "== E. Defence in depth: bob wrongly bound into alice's namespace"
kubectl -n $NA create rolebinding zz-misconfigured-bob --clusterrole=sandbox-developer --user=$B >/dev/null
sleep 1
expect allow "(RBAC now lets bob list in alice's ns)" "" -- kubectl --as $B -n $NA get sandboxclaims
expect deny  "bob execs into alice's sandbox"     "only the owner of $NA" -- kubectl --as $B -n $NA exec "$POD" -c agent -- true
expect deny  "bob deletes alice's claim"          "belongs to another developer" -- kubectl --as $B -n $NA delete sandboxclaim "$SBX" --dry-run=server
expect deny  "bob creates a claim in alice's ns"  "belongs to another developer" -- sh -c "$(declare -f claim); claim $NA t5 $B | kubectl --as $B create --dry-run=server -f -"
kubectl -n $NA delete rolebinding zz-misconfigured-bob >/dev/null

echo "== F. Platform identities are exempt (admin exec)"
expect allow "cluster admin execs into alice's sandbox" "" -- kubectl -n $NA exec "$POD" -c agent -- true

echo "== G. LLM gateway key (developer.sh issues it; the template injects it)"
expect deny  "alice reads her own litellm-key Secret" "forbidden" -- kubectl --as "$A" -n $NA get secret litellm-key
expect deny  "bob reads alice's litellm-key Secret"   "forbidden" -- kubectl --as "$B" -n $NA get secret litellm-key
# The key must pass LiteLLM auth. With no Claude login the request then fails
# closed at the provider step ("Missing Anthropic API Key"), not at the gateway.
expect allow "sandbox key is accepted by the gateway" "" -- kubectl -n $NA exec "$POD" -c agent -- sh -c \
  'curl -s -m 30 -H "x-litellm-api-key: Bearer $LITELLM_API_KEY" -H content-type:application/json \
     -d "{\"model\":\"claude-haiku-5-5\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
     $ANTHROPIC_BASE_URL/v1/messages | grep -q "Missing Anthropic API Key"'
expect allow "Claude Code request headers carry the key and the sandbox tag" "" -- kubectl -n $NA exec "$POD" -c agent -- sh -c \
  'echo "$ANTHROPIC_CUSTOM_HEADERS" | grep -q "^x-litellm-api-key: Bearer sk-" && echo "$ANTHROPIC_CUSTOM_HEADERS" | grep -qx "x-litellm-tags: sandbox:$SANDBOX_NAME"'

echo "== $pass passed, $fail failed"
[[ $fail -eq 0 ]]
