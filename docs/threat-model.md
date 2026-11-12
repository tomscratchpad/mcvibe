# Threat model

**Assumption: the agent is compromised.** Anything the agent reads (a repo,
an issue, a web page, a dependency's README, a tool result) can contain a prompt
injection, and model output can be wrong or malicious. The platform therefore
does not rely on the agent behaving well. It limits what a misbehaving agent can
**reach**, **spend**, and **take out**.

Legend: **[demo]** enforced in the prototype and covered by a test ·
**[design]** described, not built.

## What a compromised session can reach

| From a session pod | Result | Enforced by |
|---|---|---|
| Host kernel | gVisor user-space kernel (Kata VM available) | RuntimeClass + admission **[demo]** |
| Kubernetes API, kubelet, node IPs, metadata server | blocked; no SA token mounted | NetworkPolicy + VAP **[demo]** |
| Other sessions, other cluster services, Postgres | blocked | default-deny NetworkPolicy **[demo]** |
| LiteLLM `:4000` | allowed, **with its own budgeted, expiring virtual key** | NetworkPolicy + LiteLLM **[demo]** |
| Anthropic sign-in hosts (`platform.claude.com`, `claude.ai`, `claude.com`) | allowed via proxy (SNI-checked) | egress proxy **[demo]** |
| `api.anthropic.com` directly (bypass gateway) | **subscription mode: allowed** (Claude Code sign-in needs it), so the gateway is the default path, not a hard boundary; API-key mode: blocked | proxy allowlist, mode-dependent **[demo]** |
| Any other internet host (GitHub, npm, attacker.com) | blocked | proxy + NetworkPolicy **[demo]** |
| Cluster DNS | allowed (needed for `litellm`/`egress-proxy` names); **forwards external names** | known gap, see Exfiltration |

Secrets **inside** the session: the session's LiteLLM virtual key, and (in
subscription mode) the engineer's Claude OAuth token after `/login`. Nothing else:
no SA token, no provider API key, no git credentials (code moves through `sbx`).

---

## 1. Prompt injection

*Scenario:* a cloned repo or fetched page tells the agent to dump secrets, push
malicious code, or call internal systems.

- We assume it will sometimes succeed, so the controls are about **capability**,
  not detection:
  - nothing worth stealing in the sandbox (sections 2 and 3);
  - nowhere to send it (section 3);
  - no internal systems in reach (section 7);
  - its code changes only leave through review (section 5).
- **[demo]** Managed Claude Code settings (`/etc/claude-code`, read-only ConfigMap)
  override user and project settings, so a malicious repo's `.claude/settings.json`
  cannot change the model endpoint, proxy, or telemetry settings. Even if it did,
  NetworkPolicy leaves nowhere else to go.
- **[demo]** `/workspace` and `/workspace/repo` are pre-trusted in the image
  (no trust dialog). Accepted: the dialog asks a human whether to load the
  repo's project settings, but here managed settings already drop its hooks and
  permission rules, project MCP servers still need approval, and the agent can
  run arbitrary commands in the sandbox anyway. The sandbox is the boundary.
- **[demo]** Permission modes (managed settings): inside the sandbox the agent
  runs commands and edits freely (`acceptEdits` + `Bash` allowed), because the
  sandbox is the boundary. Bypass mode is disabled. Deny rules cover the OAuth
  credential file, `git push`, web tools, and `RemoteTrigger` (it could start a Claude cloud agent outside the sandbox via the allowed `claude.ai` host). A repo cannot add allow rules or
  hooks (`allowManagedPermissionRulesOnly`, `allowManagedHooksOnly`). Deny
  rules are guardrails, not a boundary. See [policies README](../k8s/policies/README.md#permission-modes-claude-code).
- **[design]** Actions with effects outside the sandbox (push, PR, deploy,
  ticket updates) go through platform tools with human-approval gates.
- **[design]** LiteLLM guardrail hooks (prompt-injection classifiers, secret
  scanning on requests and responses). They can catch some attacks, but they are
  not treated as a boundary.

## 2. Credential theft

| Credential | Where it lives | If stolen |
|---|---|---|
| LiteLLM virtual key | developer sandboxes: env var from the namespace's `litellm-key` Secret (per developer); `agent-sessions`: `~/.claude/settings.json` (per session) | **useless outside the cluster** (LiteLLM only accepts session-namespace traffic), capped by budget and rate, revocable; per-session keys also expire **[demo]** |
| Claude OAuth token (subscription mode) | session, after `/login`, until the session is deleted | **valuable**: usable from anywhere against the engineer's plan, until it expires or is revoked. Mitigations: there is little path out (section 3); sessions are disposable; the engineer can sign out or the admin can revoke. **API-key mode removes this credential from the sandbox entirely** (recommended for production) |
| Provider API keys (API-key mode) | only LiteLLM Secret | never in a session **[design-ready]** |
| LiteLLM master key, DB password | Secrets in `llm-gateway`, generated at install | requires cluster RBAC |
| Kubernetes credentials | not mounted (`automountServiceAccountToken: false`, enforced by VAP) **[demo]** | n/a |
| Git credentials | **none in the sandbox** **[demo]**: code is copied in and out by `sbx` over exec | **[design]** a short-lived, repo-scoped GitHub App token held by the L7 git gateway, never by the sandbox; push only to `agent/*` branches, no merge rights |

**Token substitution (subscription mode).** LiteLLM forwards any client OAuth
token, so an injected agent could send an *attacker's* token and have LiteLLM
deliver the conversation into the attacker's account. Closed in API-key mode;
design mitigation: pin the token hash per session key (see llm-gateway README).

## 3. Exfiltration

| Channel | Status |
|---|---|
| Direct internet (any IP/port) | **blocked** by NetworkPolicy **[demo]** |
| HTTPS to arbitrary hosts via proxy | **blocked**: allowlist + SNI check **[demo]** |
| Model API outside the gateway | subscription mode: reachable with the engineer's token (Anthropic only, unmetered by LiteLLM); API-key mode: **blocked** **[demo]** |
| Through LiteLLM to Anthropic | allowed by design (the agent must talk to the model); logged per session. With an attacker's OAuth token this becomes exfiltration (above) |
| Sign-in hosts (`claude.ai` …) | allowed; destination is Anthropic, byte counts logged. Claude Code's `RemoteTrigger` (cloud agents) is denied in managed settings **[demo]** |
| **DNS queries via CoreDNS** | **open, accepted for now**: low bandwidth. Close it with a session-only resolver that does not forward, or Cilium DNS policy ([policies README](../k8s/policies/README.md#known-gaps)) |
| Other pods / services as relays | blocked **[demo]** |
| Code pushed to git | **[demo]** no GitHub access and no git credentials in the sandbox: code comes back only through `sbx pull` into a local branch the engineer reviews (hooks and config are not transferred). **[design]** L7 git gateway for allowlisted repos, pushes to `agent/*` only, reviewed PRs (section 5) |

## 4. Untrusted dependencies

*Scenario:* the agent (or a prompt injection) installs a typo-squatted or
compromised package whose install script steals credentials or opens a backdoor.

- **[demo]** Sessions cannot reach public registries at all, so an install
  script has nothing to download from or report to.
- **[design]** Dependencies come from an internal mirror (Artifactory, Nexus, or
  similar) through the proxy allowlist. The mirror enforces a package allowlist,
  a minimum package age (e.g. 7-14 days, as used for every image in this repo),
  vulnerability and malware scanning, and lockfiles with integrity hashes.
- **[demo]** The platform's own supply chain: every image is pinned by digest or
  exact version and is at least two weeks old; the gVisor tarball is
  SHA-512-verified; and `runsc` is prevented from downloading binaries at runtime.
- **[demo]** Session pods may only use images from the platform registry
  (admission policy). **[design]** Signature verification (sigstore
  policy-controller).

## 5. Insecure agent-written code

*Scenario:* the agent writes vulnerable code, hard-codes secrets, weakens tests,
or (under injection) inserts a backdoor.

- The sandbox contains the damage while the code runs. The real risk is merging
  it. **[design]**
  - the agent pushes only to `agent/*` branches via a scoped token;
  - branch protection requires human review and passing CI;
  - CI runs SAST, secret scanning, dependency review and tests on a separate
    runner, never inside the agent sandbox;
  - CODEOWNERS covers security-sensitive paths;
  - PRs are labelled as agent-authored, with a link to the session's transcript
    and its LiteLLM spend log.
- Agent-written code is never executed outside a sandbox before review.

## 6. Runaway spend

| Layer | Control | Status |
|---|---|---|
| Request | 60 rpm, 2M tpm, 4 parallel per session key | **[demo]** rate limit tested |
| Session | `$20` key budget, 8 h key expiry | **[demo]** budget + expiry tested |
| Engineer | `$50/day` per developer key (developer namespaces) / per user (`agent-sessions`) | **[demo]** configured |
| Platform | `$200/day` global LiteLLM budget | **[demo]** configured |
| Compute | namespace quota: max 4 sessions, 6 CPU / 6 GiB; LimitRange per container | **[demo]** |
| Lifetime | hard expiry per sandbox (`sbx create --ttl`, default 8 h, SandboxClaim `shutdownTime`) | **[demo]**; idle reaper **[design]** |
| Subscription | in subscription mode $ budgets are an API-price meter; the engineer's plan limits are the real ceiling, and parallel sessions draw from one person's quota | documented |

## 7. Access to internal tools

*Scenario:* the agent is tricked into reading the internal wiki, querying
production, or calling admin APIs.

- **[demo]** No route exists. Sessions reach only DNS, LiteLLM, and the
  sign-in proxy. The cluster API and node are blocked; other namespaces are
  blocked.
- **[design]** Internal tools are exposed **only** as MCP servers behind the
  gateway (LiteLLM MCP gateway), so the virtual key that already scopes models
  also scopes tools: a per-key allowlist of tools, read-only by default, write
  tools behind human approval, every call logged and attributed. claude.ai
  connectors are disabled in managed settings (`disableClaudeAiConnectors`), so
  tools cannot appear around this path.
- Tool credentials stay in the MCP server, never in the session (same pattern as
  model keys).

## Residual risks (accepted or open)

1. DNS exfiltration through CoreDNS (low bandwidth). Open, accepted.
2. Subscription mode: OAuth token present in the sandbox; token substitution
   through LiteLLM; and direct `api.anthropic.com` access that bypasses
   gateway budgets. All three are removed by API-key mode.
3. Domain fronting via sign-in hosts (no TLS interception).
4. gVisor is not a VM boundary. Use the Kata template for high-sensitivity work.
5. Single-node prototype: no HA, no node pool separation between sessions and
   platform services. Production puts sessions on dedicated, tainted nodes.
