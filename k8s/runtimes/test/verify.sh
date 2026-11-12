#!/usr/bin/env bash
# Start one Sandbox per runtime and prove which isolation boundary each one got.
#   runc   : shares the host kernel        -> uname -r == host kernel
#   gvisor : user-space kernel (Sentry)    -> dmesg shows gVisor boot messages
#   kata   : lightweight VM with own kernel -> uname -r != host kernel
set -euo pipefail
NS="${NS:-testing}"
HERE="$(cd "$(dirname "$0")" && pwd)"

kubectl -n "$NS" apply -f "$HERE/sandboxes.yaml"
for s in rt-runc rt-gvisor rt-kata; do
  kubectl -n "$NS" wait --for=condition=Ready "sandbox/$s" --timeout=300s
done

echo "host kernel: $(uname -r)"
for s in rt-runc rt-gvisor rt-kata; do
  rc="$(kubectl -n "$NS" get pod "$s" -o jsonpath='{.spec.runtimeClassName}')"
  echo "=== $s (runtimeClassName=${rc:-<none>})"
  kubectl -n "$NS" exec "$s" -- sh -c '
    echo "kernel : $(uname -r)"
    echo "dmesg  : $(dmesg 2>/dev/null | head -1 || echo n/a)"'
done

[[ "${KEEP:-0}" == 1 ]] || kubectl -n "$NS" delete -f "$HERE/sandboxes.yaml" --wait=false
