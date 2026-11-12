# Developer namespaces, RBAC and owner admission

Each developer gets **one namespace** (`sbx-<slug>`) holding only their
sandboxes. Access is enforced twice, by independent mechanisms:

1. **RBAC:** the `sandbox-developer` ClusterRole is bound to the developer *in
   their namespace only*.
2. **Admission:** two ValidatingAdmissionPolicies compare the namespace's owner
   annotation (set by the platform) with `request.userInfo.username`. This applies
   to SandboxClaim create/update/delete and to `pods/exec|attach|portforward`.

Either one alone isolates developers. Together, a mistaken RoleBinding still
does not open another developer's sandbox (tested, section E below).

Status: **applied and tested** on the demo cluster, with impersonated
developers `alice@corp.com` and `bob@corp.com`. There is no IdP yet: OIDC login is a
placeholder in `cli/internal/auth`.

```
k8s/developers/
├── cluster/                              applied once (cluster-scoped)
│   ├── 10-clusterrole-sandbox-developer.yaml
│   └── 20-vap-developer-owner.yaml       2 policies + bindings
├── base/                                 one developer namespace (kustomize base)
├── users/<slug>/kustomization.yaml       generated per developer, committed (IaC)
├── developer.sh                          add | render | rotate-key | remove | ns <username>
└── test/
    ├── verify.sh                         26 isolation + gateway key checks
    └── permissions.sh                    14 Claude Code permission policy checks
```

## Setup

```shell
kubectl apply -f cluster/
./developer.sh add alice@corp.com     # writes users/alice/, applies it -> namespace sbx-alice
./developer.sh add bob@corp.com
sbx --as alice@corp.com create demo   # (--as: demo impersonation, see cli/README.md)
test/verify.sh demo
./developer.sh remove alice@corp.com  # offboarding: deletes the namespace and ALL its sandboxes
```

`<username>` must be exactly what the API server authenticates. With the
planned OIDC setup that is e.g. `oidc:alice@corp.com`. The namespace name follows
the same rule as the CLI (`sbx whoami` prints it): drop an `idp:` prefix and
`@domain`, lowercase, `[a-z0-9-]`, at most 20 chars, then prefix `sbx-`.
`developer.sh` refuses a slug collision (another owner already holds `sbx-alice`).

**IaC flow:** onboarding is a PR adding `users/<slug>/` (`developer.sh render`),
reviewed like any change; CI runs `kubectl apply -k users/<slug>`. Offboarding
is the PR removing it, plus deleting the namespace.

## What a developer namespace contains (`base/`)

| Object | Purpose |
|---|---|
| Namespace | labels `agent-platform/sessions=true` (PSA `restricted`, existing secure-sandbox and session-pod VAPs, LiteLLM and egress-proxy ingress), `agent-platform/tenant=developer` (owner VAPs). Annotation `agent-platform/owner-username` |
| ResourceQuota + LimitRange | per-person cap: 2 pods, 4 CPU / 4 GiB limits, no PVC/LB/NodePort. With one namespace per developer, the quota is a per-person spend and blast-radius limit |
| NetworkPolicy `default-deny` | non-template pods get no network |
| SandboxTemplate + WarmPool `claude-code` | same as the platform template (`k8s/policies/40-…`): gVisor, egress only to DNS, LiteLLM and the egress proxy. Image `localhost:5000/devbox:3` |
| ConfigMap `claude-code-managed-settings` | same managed Claude Code settings as `agent-sessions` |
| RoleBinding `sandbox-developer` | ClusterRole → this one user, this namespace only |

The template and settings are **copies** of `k8s/policies/40-*` and `41-*` with
"keep in sync" headers. Production: generate both from one source, or have
CI diff them.

## LLM gateway key

Each developer gets **one LiteLLM virtual key**, issued by the platform at
onboarding. Nothing runs per sandbox: no controller, no exec, nothing in `~/.claude`.

```
developer.sh add|rotate-key  ──(admin: master key)──▶ LiteLLM /key/generate  alias dev-<slug>, user_id=<username>
             └─▶ Secret litellm-key in sbx-<slug>
SandboxTemplate (per namespace) ──secretKeyRef──▶ env LITELLM_API_KEY
   ANTHROPIC_CUSTOM_HEADERS = "x-litellm-api-key: Bearer $(LITELLM_API_KEY)
                               x-litellm-tags: sandbox:$(SANDBOX_NAME)"
```

| Property | |
|---|---|
| Who can read it | platform admins. Developers have **no RBAC on Secrets** (tested: alice cannot read her own, bob cannot read alice's). The developer's own sandboxes see it as an env var, which is unavoidable for any credential an agent uses |
| Where it works | only from session namespaces: LiteLLM's NetworkPolicy. A leaked key is useless outside the cluster |
| Limits | Claude models only, **$50/day** (API-price meter), 120 rpm, 4M tpm, 8 parallel requests |
| Attribution | `user_id` = the developer; each request is tagged `sandbox:<name>` (spend logs show `user=alice@corp.com … tags=['sandbox:keytest']`). The tag is set by the sandbox, so it is attribution, not a security boundary |
| Revocation | `developer.sh remove` (offboarding) deletes the key before the namespace. `rotate-key` replaces it, and running sandboxes keep the old key until recreated |
| Fail closed | `secretKeyRef` is not optional: a namespace without the Secret cannot start sandboxes |

**Why this, and not the alternatives:**

| Option | Verdict |
|---|---|
| **Per-developer key in a namespace Secret, injected by the template** | **chosen.** Uses only Kubernetes primitives, and the per-namespace template already exists. The only privileged step is onboarding, which already needs admin |
| Controller that issues a per-session key on each Ready claim and installs it over exec | more precise (per-session budget and expiry), but a new always-on component holding the LiteLLM master key **and** `pods/exec` in every developer namespace. That is the most powerful identity on the platform, which is worse than what it protects |
| LiteLLM JWT auth with audience-bound projected ServiceAccount tokens | no stored key at all, but it is a LiteLLM enterprise feature, and it needs a VAP exception for projected tokens |
| No gateway auth for developer namespaces | loses budgets, rate limits and attribution, and the only gain is skipping one onboarding step |

`agent-sessions` (platform-run sessions) keeps per-session keys via
`k8s/llm-gateway/session-key.sh`.

Tests: `test/verify.sh` section G (Secret unreadable by alice and bob, the key
passes gateway auth, the headers carry the key and the sandbox tag).

## RBAC: `sandbox-developer`

| Resource | Verbs | Used by |
|---|---|---|
| `sandboxclaims` | create, get, list, delete | `sbx create/list/delete/prune` |
| `sandboxes` | get, list | claim → Sandbox → `status.selector` |
| `pods` | get, list | resolve the pod, readiness |
| `pods/exec` | create, get | `sbx exec` / `sbx shell` / herdr-core transport. `get` = WebSocket exec, `create` = SPDY |

Not granted: Sandboxes/Templates/WarmPools/Pods create or update (the pod spec
is platform-owned), `pods/attach`, `pods/portforward`, `pods/log`, secrets,
configmaps, services, networkpolicies, namespaces, anything cluster-scoped, and
anything outside the developer's own namespace (`agent-sessions`, `llm-gateway`).

## Admission: `cluster/20-vap-developer-owner.yaml`

Bound to namespaces labelled `agent-platform/tenant=developer`; `failurePolicy: Fail`.

| Policy | Operations | Rule |
|---|---|---|
| `developer-sandboxclaim-owner` | SandboxClaim CREATE / UPDATE / DELETE | the namespace owner annotation must equal the caller; the claim's `agent-platform/owner-username` must equal the caller; the owner is immutable on UPDATE |
| `developer-exec-owner` | `pods/exec`, `pods/attach`, `pods/portforward` CONNECT | the namespace owner annotation must equal the caller |

These checks use data RBAC cannot see. Developers have no rights on
namespaces, so they cannot change the owner annotation. Exempt: `system:*`
identities (controllers, garbage collector, namespace deletion, cluster admins)
and the IdP group `oidc:sandbox-admins`. RBAC still applies to them.

## Test results (`test/verify.sh`)

```
== A. owner uses their own namespace
  PASS  alice lists her claims / execs into her sandbox / creates a claim
== B. RBAC: another developer is locked out
  PASS  bob lists / execs into / deletes / creates claims in alice's ns   [forbidden]
== C. RBAC: what alice may not do even in her own namespace
  PASS  bare Sandbox, Pod, port-forward, edit template, edit namespace, secrets,
        exec in agent-sessions, exec in llm-gateway                       [denied]
== D. Admission: claim owner must be the caller
  PASS  alice creates a claim owned by bob / with no owner     [must be your own username]
== E. Defence in depth: bob wrongly bound into alice's namespace
  PASS  (RBAC now lets bob list in alice's ns)
  PASS  bob execs into alice's sandbox                         [only the owner of sbx-alice]
  PASS  bob deletes / creates claims in alice's ns             [belongs to another developer]
== F. Platform identities are exempt
  PASS  cluster admin execs into alice's sandbox
== G. LLM gateway key
  PASS  alice / bob read alice's litellm-key Secret            [forbidden]
  PASS  sandbox key is accepted by the gateway
  PASS  Claude Code request headers carry the key and the sandbox tag
== 26 passed, 0 failed
```

## The gap this closes

Before this, all sandboxes shared `agent-sessions` with one namespaced Role.
RBAC cannot scope by owner, so any developer could:
- exec into a colleague's sandbox: read code and transcripts, type into their
  agent's terminal (prompt injection by a colleague), plant files;
- exec into an unclaimed **warm-pool** pod and leave a backdoor for the next
  claimant;
- delete a colleague's sandbox, or create claims labelled as someone else;
- list everyone's sandboxes.

All of these are now denied by RBAC, and again by admission (tests B, D, E).
Warm pools are per namespace, so a developer's warm pods can only ever be
claimed by that developer.

The blast radius was always lateral (between colleagues), never into the
infrastructure. A sandbox is UID 1000 under gVisor with PSA `restricted`, no SA
token, and egress only to DNS, LiteLLM and the egress proxy.

## Remaining risks and next steps

- **Open exec streams outlive revocation.** Disabling a user at the IdP stops
  *new* API calls, but an established exec stream (herdr-core connection,
  `sbx shell`) stays open until it drops. Kill switch today: delete the claim or
  the namespace. A session broker that owns `pods/exec` would give instant
  revocation plus session recording.
- **`system:*` exemption is broad.** It is safe while no human holds a service
  account token: developers cannot create SAs or TokenRequests, and sandboxes have
  `automountServiceAccountToken: false`. Production: exempt only the named
  controller SAs.
- **Template drift:** the per-namespace copies of the template and settings (see above).
- **Gateway key is per developer, long-lived.** It is not per session and has
  no expiry (see "LLM gateway key" above). Next steps: rotate on a schedule
  (`rotate-key` from CI), and keep the secret in a secret manager (External
  Secrets) rather than created imperatively by `developer.sh`.
- **Namespace per developer costs objects:** about 8 per developer, plus
  controller-managed NetworkPolicies. That is fine for hundreds of developers.
  For thousands, use a tenancy operator (Capsule, HNC) instead of rendered overlays.
- **Audit:** set an audit policy at level `Metadata` for `pods/exec` and
  `sandboxclaims`, so every remote session and lifecycle action is attributable.
