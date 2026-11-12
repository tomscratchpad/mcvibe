# Kubernetes platform

Everything that runs in the cluster. Install in this order:

| # | Directory | What | Install |
|---|---|---|---|
| 0 | (host) | k3s, docker + buildx, a local registry (`localhost:5000`, see [../image](../image/README.md)) | k3s installer |
| 1 | (upstream) | [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) **v1.0.10**: controller + CRDs and extensions (`SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`). It manages the pod lifecycle; isolation comes from the runtime and the policies below | upstream release manifests |
| 2 | [runtimes/](runtimes/README.md) | gVisor (default) and Kata RuntimeClasses | `sudo gvisor/install.sh`, `kata/install.sh` |
| 3 | [policies/](policies/README.md) | namespaces, Pod Security, admission policies, quotas, NetworkPolicies, the `claude-code` template, Claude Code managed settings | `kubectl apply -f policies/00-namespaces.yaml && kubectl apply -f policies/` |
| 4 | [egress-proxy/](egress-proxy/README.md) | Envoy: the only route to the internet (hostname + SNI allowlist) | `kubectl apply -k egress-proxy` |
| 5 | [llm-gateway/](llm-gateway/README.md) | LiteLLM + Postgres: keys, budgets, spend logs, admin UI | `llm-gateway/install.sh` |
| 6 | [developers/](developers/README.md) | one namespace per developer, RBAC + owner admission, gateway key | `kubectl apply -f developers/cluster/`, `developers/developer.sh add <user>` |

Namespaces:

| Namespace | Holds |
|---|---|
| `sbx-<developer>` | that developer's sandboxes (created with `sbx`, see [../cli](../cli/README.md)) |
| `agent-sessions` | platform-run sessions (automation), per-session gateway keys (`llm-gateway/session-key.sh`) |
| `llm-gateway` | LiteLLM, Postgres |
| `egress-proxy` | Envoy |
| `agent-sandbox-system` | the agent-sandbox controller |

Test suites run against the live cluster: `runtimes/test`, `policies/test`,
`llm-gateway/test` (also covers the egress proxy), `developers/test`. Threats and
controls across all of them: [../docs/threat-model.md](../docs/threat-model.md).

**Demo notes:** k3s rewrites `/etc/rancher/k3s/k3s.yaml` (root-only) on restart;
use a copy at `~/.kube/config`. Developer laptops need only `sbx` and the Herdr
app ([../docs/developer-guide.md](../docs/developer-guide.md)).
