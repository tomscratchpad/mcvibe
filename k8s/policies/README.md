# Agent session policies

Guardrails for the namespace where agent sandboxes run, and the network
skeleton for the LLM gateway (LiteLLM).

```shell
kubectl apply -f 00-namespaces.yaml && kubectl apply -f .
test/verify.sh            # 20 checks; KEEP=1 keeps the test pods
```

## Layers

Each layer covers a gap in the others. All of them are enforced in the cluster today.

| # | File | Mechanism | What it stops |
|---|------|-----------|---------------|
| 1 | `00-namespaces.yaml` | Pod Security Admission `restricted` on `agent-sessions` | root, privilege escalation, added caps, host namespaces/paths, non-default seccomp, on **every pod** |
| 2 | `10-quota.yaml` | ResourceQuota + LimitRange | runaway resource use: max 4 pods / 6 CPU / 6 GiB, no LoadBalancer/NodePort services |
| 3 | `20-vap-secure-sandbox.yaml` | ValidatingAdmissionPolicy on `Sandbox` **and** `SandboxTemplate` | non-gVisor/Kata runtime, SA tokens, privileged, hostPath/hostNetwork/hostPort, missing limits, ... |
| 4 | `21-vap-session-pods.yaml` | ValidatingAdmissionPolicy on `Pod` | creating a pod directly (bypassing Sandbox) on runc, or pulling images from outside the platform registry |
| 5 | `31-netpol-agent-sessions.yaml` | namespace default-deny NetworkPolicy | any pod not created from a template gets no network at all |
| 6 | `40-sandboxtemplate-claude-code.yaml` | the single session template (`claude-code`, gVisor) + controller-managed NetworkPolicy | egress limited to **cluster DNS, LiteLLM :4000, egress proxy :3128** (sign-in hosts only); ingress only from the Sandbox Router |
| 6b | `41-claude-code-managed-settings.yaml` | Claude Code managed settings (read-only mount) | a cloned repo's `.claude/settings.json` cannot change the model endpoint, proxy or telemetry |
| 7 | `30-netpol-llm-gateway.yaml` | NetworkPolicy on `llm-gateway` | only session pods reach LiteLLM; LiteLLM reaches only DNS, its Postgres and the egress proxy |

`50-warmpools.yaml`: in agent-sandbox v1.0.x, SandboxClaims must reference a warm
pool. The pool uses `replicas: 0` here, so each claim cold-starts from the template.

`examples/sandboxtemplate-claude-code-kata.yaml`: the same template on Kata
Containers, plus its pool. It is not applied by `kubectl apply -f .`; see "Runtime choice".

## Runtime choice: gVisor (Kata available)

There is **one session template, `claude-code`, and it runs on gVisor.** Both
runtimes are installed and allowed by the admission policies (see
[../runtimes/README.md](../runtimes/README.md)).

| | gVisor (`claude-code`) | Kata (`examples/`) |
|---|---|---|
| Boundary | user-space kernel (Sentry) intercepts syscalls; the host kernel sees a small, seccomp-filtered syscall set | separate guest kernel in a microVM; boundary is hardware virtualization (KVM) |
| Cold start (demo node, cached image) | **~0.6 s** | ~5 s |
| Memory overhead | tens of MiB | 320 MiB declared overhead + ~270 MiB idle QEMU RSS |
| Node requirements | any Linux node, no KVM | `/dev/kvm`: bare metal or nested-virt instances |
| Compatibility | most dev tooling works; some syscalls/ioctls unimplemented, slower syscall- and I/O-heavy workloads | full Linux kernel; can run nested containers |

**Why gVisor by default:** for performance and density. Sessions start in under
a second and cost little memory, so engineers can run many parallel sessions per
node. It runs on ordinary cloud VMs (no nested virtualization or metal instances)
and is a managed option on GKE (GKE Sandbox). gVisor is a large improvement over
runc: a kernel exploit in the agent's code has to beat the Sentry first, and then
the narrow host syscall filter.

**When to use Kata:** when a workload needs a hardware-virtualization boundary,
e.g. highly sensitive repositories, running untrusted third-party code, or a
workload gVisor does not support (Docker-in-sandbox, unusual syscalls). Kata is
installed and tested on the cluster. Enabling it is
`kubectl apply -f examples/`. The template differs only in `runtimeClassName`.
In production this would be a second, separately approved template on a
KVM-capable node pool, not a per-session switch engineers can flip.

### Reused vs written

- **Adapted from upstream** agent-sandbox (`examples/policy/vap`, commit `890de53`):
  the Sandbox VAP. Changes: allow `gvisor` *or* `kata`; also validate
  `SandboxTemplate`; require RuntimeDefault seccomp; drop the GKE-only nodeSelector
  and toleration rules; bind by namespace label instead of "all but kube-system".
- **Used as designed:** template-level managed NetworkPolicy
  (`examples/policy/network-policy-management`). The controller generates
  `<template>-network-policy` and selects pods by template hash.
- **Written here:** the pod-level VAP (bypass and registry), namespace default-deny,
  PSA labels, quota, the LLM-gateway policies, and the test suite.

## Decisions and trade-offs

- **No direct internet from sessions, even for model APIs.** Model traffic goes
  through LiteLLM ([../llm-gateway](../llm-gateway/README.md)), which enforces
  per-developer budgets and logs every call. The only other way out is the
  hostname-allowlisting [egress proxy](../egress-proxy/README.md), which allows
  only Claude sign-in hosts. Code moves through `sbx` (git bundles over exec);
  packages would come from an internal mirror.
- **Custom template rules over the controller's "secure default".** The secure
  default allows all of the public internet and points DNS at 8.8.8.8 and 1.1.1.1.
  Custom rules give us a deny-all-but-gateway posture and keep cluster DNS, which
  is needed to resolve the gateway.
- **Admission on templates, not only Sandboxes.** A bad template now fails at
  `kubectl apply`. Otherwise it would only fail later, inside the controller, when
  someone claims a session.
- **The pod VAP is a backstop.** Anyone able to create Pods in the namespace would
  otherwise skip the Sandbox policy. PSA does not check the runtime class or the
  image source.
- **`failedPodPolicy: Retain`.** An evicted or failed session is kept for
  inspection instead of being silently replaced by an empty pod.
- **One template.** One approved blueprint keeps the security review surface
  small. Variants (Kata, bigger resources) are separate, reviewed templates.
- **Keep cluster DNS for now (accepted risk).** See "Known gaps".

## Permission modes (Claude Code)

Set in managed settings (`41-claude-code-managed-settings.yaml`, copied in
`../developers/base/`), which a user or a cloned repository cannot override.

| Setting | Value | Why |
|---|---|---|
| `defaultMode` | `acceptEdits` | Remote sessions run while nobody watches. A prompt per edit or command would block them. |
| `allow` | `Bash` | Commands run without a prompt. **The sandbox is the boundary** (gVisor, no credentials worth stealing, egress allowlist), not per-command approval. |
| `disableBypassPermissionsMode` | `disable` | Autonomy is set by the platform, not by a `--dangerously-skip-permissions` flag (the session silently starts in `acceptEdits` instead; tested). Deny rules and future `ask` rules keep working. |
| `deny` | OAuth credential file (`Read`, `Edit`, `Bash(*.credentials.json*)`), `Bash(*git push*)`, `WebFetch`, `WebSearch` | Guardrails against the obvious prompt-injection moves. Web tools are denied because the sandbox has no internet by design and web pages are the main injection source. The sandbox has no git remote; code leaves via `sbx pull`. |
| `deny` | `RemoteTrigger` | Found by the tests: this tool manages Claude **cloud** agents through `claude.ai`, which is on the sign-in allowlist. An injected agent could start an agent outside our sandbox, with internet access. A tool-wide deny removes it from the agent's tool list entirely. |
| `allowManagedPermissionRulesOnly` | `true` | A repo's `.claude/settings.json` cannot add allow rules (e.g. `WebFetch`). |
| `allowManagedHooksOnly` | `true` | A repo's hooks do not run. Only the platform's rtk hook does. |
| `forceLoginMethod` | `claudeai` | Subscription sign-in only, matching the gateway's subscription mode. Production adds `forceLoginOrgUUID` (the company's Claude organization) so a personal or attacker account is refused at login. |

**Deny rules are guardrails, not a boundary.** Bash can always obfuscate
(`sh -c`, base64, a script file). They block the plain attempt and show intent
in the transcript. What actually protects the credential is that it has nowhere
to go (egress allowlist) and, in API-key mode, that it does not exist in the
sandbox at all.

**rtk interaction.** The rtk PreToolUse hook rewrites commands *before* the
rules are checked (`git push` → `rtk git push`, `cat f` → `rtk read f`; tested
with `rtk hook claude`). It does not auto-approve anything. Bash deny rules
therefore use `*` on both sides; a rule such as `Bash(git push:*)` would never match.

**Engineer experience.** In practice the agent never stops for permission inside
its sandbox. It is refused when it touches the credential, pushes, or tries
the web. Codex is not implemented in this demo; the design maps it to `sandbox_mode = "danger-full-access"`
+ `approval_policy = "never"` for the same reason: the pod is the sandbox.

Tests: `../developers/test/permissions.sh` runs real headless Claude Code in a
signed-in sandbox (Haiku, a few cents). It reads the session's init event
(permission mode, tool list) and the `permission_denials` of each run:

```
== A. modes (bypass flags fall back to the managed default)    3 PASS
== A2. tools removed by tool-wide deny rules                    3 PASS (WebFetch, WebSearch, RemoteTrigger)
== B. allowed: Bash and file edits without a prompt             2 PASS
== C. denied: credential file via Bash (rtk) and Read, git push 3 PASS
== D. a cloned repo cannot loosen the policy                    3 PASS (defaultMode, allow rule, hooks)
== 14 passed, 0 failed
```

## Test results (`test/verify.sh`)

Run against a session claimed from `claude-code` (gVisor). The same checks
passed on Kata before it moved to `examples/`. The allowed path is checked
against the real LiteLLM. Control checks from an unrestricted pod show that the
BLOCKED results come from the policy, not from the network.

```
A. 7/7 insecure Sandbox/Template/Pod specs rejected (each by the intended policy)
B. session (gVisor): DNS ok, LiteLLM:4000 OPEN;
   k8s API (svc + node IP), kubelet, metadata IP, other pods, internet,
   direct external DNS: BLOCKED
   INFO known gap: api.anthropic.com resolves via CoreDNS
   controls: unrestricted pod -> internet / k8s API: OPEN
C. outsider -> LiteLLM, outsider -> session: BLOCKED
== 20 passed, 0 failed
```

## Known gaps

- **DNS exfiltration through CoreDNS (verified; accepted for now).**
  - *What:* sessions may only talk to CoreDNS, but CoreDNS forwards any
    non-cluster name to the upstream resolvers. A compromised or prompt-injected
    agent can encode data in query names (`<secret-chunk>.attacker.example`), and
    the attacker's authoritative server receives them. `test/verify.sh` prints
    this on every run (`INFO known gap`). Direct queries to external resolvers
    (8.8.8.8:53) *are* blocked.
  - *Impact:* a low-bandwidth (bytes per query), noisy channel. It can still
    carry small secrets (tokens, keys) out. It cannot be used to download
    payloads or reach any other service.
  - *Why accepted for now:* sessions need name resolution for the LiteLLM and
    egress-proxy services. Removing DNS now would make the sessions
    brittle (hard-coded IPs) before the rest of the egress design exists. It is
    also mitigated by having **no real credentials in the sandbox**: model keys
    stay in LiteLLM and the sandbox has no SA token, so there is little worth
    exfiltrating (the subscription OAuth token is the exception, see the threat model).
  - *Ways to close it (later):*
    1. Pin LiteLLM's Service `clusterIP`, inject it with `hostAliases`, set
       `dnsPolicy: None`, and remove DNS egress entirely. This fully closes the
       channel, but every other destination must then be addressed by IP or go
       through a proxy.
    2. A dedicated CoreDNS for session namespaces that answers only
       `*.svc.cluster.local` (plus an allowlist), with no upstream forwarding.
    3. A CNI with DNS-aware policy (Cilium `toFQDNs` and its DNS proxy), which
       also gives FQDN egress rules.
    4. At minimum: log session DNS queries (CoreDNS `log` plugin) and alert on
       high-entropy or high-volume lookups.
- **Registry check is a prefix match.** Production needs digest pinning plus
  signature verification (sigstore policy-controller or Kyverno).
- **Kubelet and metadata checks** pass, but on this VM the kubelet is also
  host-firewalled and there is no metadata server. The tests are meaningful on a
  cloud node.
