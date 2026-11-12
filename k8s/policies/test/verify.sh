#!/usr/bin/env bash
# Verify admission + network policies for agent sessions.
#
#  A. Admission: insecure Sandboxes/Pods are rejected, a compliant claim is accepted.
#  B. Network: from inside a session pod (claude-code template, gVisor),
#     DNS + LiteLLM gateway are reachable; k8s API, node, other pods,
#     internet and external DNS are not. Plus: an outsider pod cannot reach
#     the gateway or the session.
#
# Uses the real LiteLLM gateway (../llm-gateway) for the allowed path.
# Set KEEP=1 to keep resources.
set -uo pipefail
NS=agent-sessions
GW_NS=llm-gateway
OUT_NS=testing
IMG=localhost:5000/test:1
pass=0; fail=0
ok()  { echo "  PASS  $*"; pass=$((pass+1)); }
bad() { echo "  FAIL  $*"; fail=$((fail+1)); }

# expect_deny <description> : reads a manifest on stdin, server-side dry-run must be rejected
expect_deny() {
  local desc="$1" out
  if out="$(kubectl apply --dry-run=server -f - 2>&1)"; then bad "$desc (was accepted)"; return; fi
  # Only count rejections by a policy, not schema/YAML errors.
  local why; why="$(echo "$out" | grep -oE "(denied request|violates PodSecurity).*" | head -1 | cut -c1-110)"
  [[ -n "$why" ]] && ok "$desc -> $why" || bad "$desc: rejected for another reason: $out"
}

hardened_pod_spec() { # $1 = runtimeClassName line (may be empty), $2 = image
cat <<EOF
    $1
    automountServiceAccountToken: false
    securityContext: { runAsNonRoot: true, runAsUser: 1000, seccompProfile: { type: RuntimeDefault } }
    containers:
    - name: c
      image: $2
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ["ALL"] } }
      resources: { limits: { cpu: 500m, memory: 256Mi } }
EOF
}

echo "== A. Admission policies"
expect_deny "Sandbox without runtimeClassName (runc)" <<EOF
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: { name: bad-runc, namespace: $NS }
spec:
  podTemplate:
    spec:
$(hardened_pod_spec "" "$IMG" | sed 's/^/    /')
EOF

expect_deny "Sandbox with privileged container" <<EOF
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: { name: bad-priv, namespace: $NS }
spec:
  podTemplate:
    spec:
      runtimeClassName: gvisor
      automountServiceAccountToken: false
      containers:
      - name: c
        image: $IMG
        securityContext: { privileged: true }
EOF

expect_deny "Sandbox with service-account token mounted" <<EOF
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: { name: bad-token, namespace: $NS }
spec:
  podTemplate:
    spec:
      runtimeClassName: kata
      automountServiceAccountToken: true
      containers: [ { name: c, image: $IMG } ]
EOF

expect_deny "SandboxTemplate using hostPath" <<EOF
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata: { name: bad-hostpath, namespace: $NS }
spec:
  podTemplate:
    spec:
        volumes: [ { name: host, hostPath: { path: / } } ]
$(hardened_pod_spec "runtimeClassName: gvisor" "$IMG" | sed 's/^/    /')
EOF

expect_deny "bare Pod without runtimeClassName (bypassing Sandbox)" <<EOF
apiVersion: v1
kind: Pod
metadata: { name: bad-pod, namespace: $NS }
spec:
$(hardened_pod_spec "" "$IMG")
EOF

expect_deny "bare Pod from untrusted registry" <<EOF
apiVersion: v1
kind: Pod
metadata: { name: bad-image, namespace: $NS }
spec:
$(hardened_pod_spec "runtimeClassName: gvisor" "docker.io/library/alpine:3")
EOF

expect_deny "bare Pod running as root (PSA restricted)" <<EOF
apiVersion: v1
kind: Pod
metadata: { name: bad-root, namespace: $NS }
spec:
  runtimeClassName: gvisor
  containers: [ { name: c, image: $IMG, securityContext: { runAsUser: 0 } } ]
EOF

echo "== Setup: outsider pod, one session claim"
# The outsider lives in a plain namespace (no session labels, no NetworkPolicy),
# standing in for any other workload on the cluster. Created here, idempotently.
kubectl create namespace $OUT_NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# Remove leftovers from a previous run and wait, so we never re-use a terminating pod.
kubectl -n $NS delete sandboxclaim nettest --ignore-not-found --wait=true >/dev/null
kubectl -n $OUT_NS delete pod outsider --ignore-not-found --wait=true >/dev/null
kubectl -n $NS wait --for=delete pod/nettest --timeout=120s >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: outsider, namespace: $OUT_NS, labels: { app: outsider } }
spec:
  containers:
  - name: c
    image: $IMG
    command: ["python3", "-m", "http.server", "8080"]
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata: { name: nettest, namespace: $NS }
spec:
  warmPoolRef: { name: claude-code }
  lifecycle: { shutdownPolicy: Delete }
EOF
kubectl -n $OUT_NS wait --for=condition=Ready pod/outsider --timeout=120s >/dev/null
kubectl -n $NS wait --for=condition=Ready sandboxclaim/nettest --timeout=300s >/dev/null
OUTSIDER_IP="$(kubectl -n $OUT_NS get pod outsider -o jsonpath='{.status.podIP}')"
NODE_IP="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"

# probe <ns> <pod> <host> <port> -> prints OPEN or BLOCKED
probe() {
  kubectl -n "$1" exec "$2" -- python3 -c "
import socket,sys
try: socket.create_connection(('$3',$4),timeout=4).close(); print('OPEN')
except Exception: print('BLOCKED')" 2>/dev/null || echo ERROR
}
check() { # <expect OPEN|BLOCKED> <ns> <pod> <host> <port> <desc>
  local got; got="$(probe "$2" "$3" "$4" "$5")"
  [[ "$got" == "$1" ]] && ok "$6: $got" || bad "$6: expected $1, got $got"
}

pod=nettest
{
  echo "== B. Network from $pod (runtimeClassName=$(kubectl -n $NS get pod $pod -o jsonpath='{.spec.runtimeClassName}'))"
  r="$(kubectl -n $NS exec $pod -- python3 -c "import socket;print(socket.gethostbyname('litellm.$GW_NS.svc.cluster.local'))" 2>&1)"
  [[ "$r" =~ ^[0-9.]+$ ]] && ok "cluster DNS resolves litellm.$GW_NS -> $r" || bad "cluster DNS: $r"
  check OPEN    $NS $pod litellm.$GW_NS.svc.cluster.local 4000 "LLM gateway :4000"
  check BLOCKED $NS $pod kubernetes.default.svc.cluster.local 443 "Kubernetes API (service)"
  check BLOCKED $NS $pod "$NODE_IP" 6443 "Kubernetes API (node IP)"
  check BLOCKED $NS $pod "$NODE_IP" 10250 "kubelet (also host-firewalled on this VM)"
  check BLOCKED $NS $pod 169.254.169.254 80 "cloud metadata IP (no metadata server on this VM)"
  check BLOCKED $NS $pod "$OUTSIDER_IP" 8080 "other pod in cluster"
  check BLOCKED $NS $pod 1.1.1.1 443 "internet (HTTPS)"
  check BLOCKED $NS $pod 8.8.8.8 53 "external DNS server, direct"
  # Accepted risk (README "Known gaps"): external names still resolve via
  # CoreDNS forwarding, i.e. DNS remains a low-bandwidth exfiltration channel.
  r="$(kubectl -n $NS exec $pod -- python3 -c "import socket;print(socket.gethostbyname('api.anthropic.com'))" 2>&1 | tail -1)"
  echo "  INFO  known gap: api.anthropic.com resolves via CoreDNS -> $r"
}

echo "== Controls (outsider pod has no NetworkPolicy: these must be OPEN, proving BLOCKED above is policy)"
check OPEN $OUT_NS outsider 1.1.1.1 443 "outsider -> internet"
check OPEN $OUT_NS outsider kubernetes.default.svc.cluster.local 443 "outsider -> Kubernetes API"

echo "== C. Ingress"
check BLOCKED $OUT_NS outsider litellm.$GW_NS.svc.cluster.local 4000 "outsider -> LLM gateway"
SB_IP="$(kubectl -n $NS get pod nettest -o jsonpath='{.status.podIP}')"
check BLOCKED $OUT_NS outsider "$SB_IP" 8888 "outsider -> session pod"

if [[ "${KEEP:-0}" != 1 ]]; then
  kubectl -n $NS delete sandboxclaim nettest --wait=false >/dev/null
  kubectl -n $OUT_NS delete pod outsider --wait=false >/dev/null
fi
echo "== $pass passed, $fail failed"
[[ $fail -eq 0 ]]
