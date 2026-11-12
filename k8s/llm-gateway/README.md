# LLM gateway (LiteLLM)

Every model request from a session goes through LiteLLM. It is where the
platform authenticates sessions, enforces spend and rate limits, attributes usage
to an engineer and session, and can cut a session off.

```shell
./install.sh                                  # Postgres + LiteLLM, generates secrets on first run
./session-key.sh issue <session> <engineer>   # per-session virtual key -> into the session pod
./session-key.sh info|revoke <session>
test/verify.sh                                # checks from inside a real session
```

Prerequisites: `../policies` (namespace, NetworkPolicies) and `../egress-proxy`.

## Request path

```
session pod (gVisor)                         llm-gateway                     egress-proxy           Anthropic
claude ──HTTP──> litellm:4000 ──────────────> LiteLLM ──CONNECT :3129──────> allowlist+SNI ──TLS──> api.anthropic.com
        x-litellm-api-key: <session key>       ├ checks key: scope, budget,
        Authorization: <engineer's OAuth>      │ rpm/tpm, parallel, expiry
                                               └ Postgres (keys, spend logs)
claude /login ──CONNECT :3128──────────────────────────────────────────────> platform.claude.com, claude.ai,
                                                                              api.anthropic.com (subscription mode only)
```

## Demonstrated end to end (subscription mode)

1. `SandboxClaim demo` (gVisor) is created; `session-key.sh issue demo engineer-1`
   installs a $20, 8-hour key.
2. The engineer attaches (`kubectl exec -it … -- claude`), runs `/login`, and
   completes Anthropic's OAuth flow in their own browser.
3. Egress proxy log for the session: only `platform.claude.com` and
   `api.anthropic.com` were allowed (sign-in). Claude Code's attempts to reach
   `downloads.claude.ai` (updater) and `github.com` were denied.
4. Prompt → LiteLLM → egress proxy (gateway listener) → Anthropic. One spend-log row:
   `claude-opus-5-5 | user engineer-1 | key session-demo | 18,642/111 tokens | $0.15 (API-equivalent) | success`.
   No Anthropic API key is configured, so the request used the engineer's
   forwarded subscription token.

## Admin UI

The LiteLLM admin UI (keys, budgets, spend logs per developer and sandbox) is
served by LiteLLM itself on `/ui`. It is not published outside the cluster: open
it from the VM through a `kubectl port-forward`, which binds to `127.0.0.1` only.

**1. Start the port-forward** on the VM and leave it running:

```shell
kubectl -n llm-gateway port-forward svc/litellm 4000:4000
```

**2. Get the login password** (the master key) on the VM:

```shell
kubectl -n llm-gateway get secret litellm -o jsonpath='{.data.master-key}' | base64 -d; echo
```

**3. Open <http://localhost:4000/ui>** and sign in with username `admin` and the
master key as password.

Useful pages: *Virtual Keys* (`dev-alice`, `dev-bob`, `session-*`: budget, spend,
block/unblock), *Usage* / *Logs* (spend per user and per `sandbox:<name>` tag),
*Models*.

**Security of this choice:**
- The UI login is as strong as the master key (`sk-` + 32 random bytes) and gives
  full admin rights, so treat the master key like a cluster-admin credential and do
  not paste it anywhere else.
- Sessions can reach LiteLLM's port, so they can see the `/ui` login page, but
  not log in without the master key. Disable it again with
  `DISABLE_ADMIN_UI=True` in `litellm.yaml` if you prefer an API-only gateway.
- Production: put the UI behind SSO (LiteLLM supports OIDC for the UI, an
  enterprise feature), or keep it on an internal admin network, never on the
  session path.

## Subscription vs API key: what we chose and why

The exercise uses a **Claude Max subscription**, so the gateway runs in
**subscription mode**. No Anthropic API key is configured.

**What the terms allow** ([Claude Code legal & compliance](https://code.claude.com/docs/en/legal-and-compliance)):
an engineer *may* sign in to the unmodified Claude Code binary with their own
subscription, including on a platform that hosts Claude Code. The platform may
**not** pool or resell usage, and "may not collect, store, or intermediate
Claude.ai credentials". Sign-in must complete through Anthropic's own flow.

**How the design follows that:**
- **Each engineer signs in with `/login` inside their own session.** The OAuth
  token lives only in that disposable sandbox and disappears with it. The
  platform never stores it: it is not in a Secret, not in the SandboxClaim, and not
  in LiteLLM's database. We rejected injecting a token made with `claude setup-token`
  into a per-session Secret: the platform would store a 1-year credential.
- **No company credential to fall back on.** A request without a valid client
  OAuth token fails (`Missing Anthropic API Key`, tested). Usage can never be
  silently billed to, or pooled under, someone else's account.
- LiteLLM forwards the client's OAuth token per request (LiteLLM ≥ 1.81.14,
  [tutorial](https://docs.litellm.ai/docs/tutorials/claude_code_max_subscription)).
  The session authenticates to LiteLLM separately with its virtual key
  (`x-litellm-api-key`).

**What subscription mode implies:**

| | Subscription (now) | API key (enable in `config.yaml`) |
|---|---|---|
| Who pays | each engineer's plan (flat), usage limits per 5-hour / weekly window | the company, per token |
| Parallel sessions | all of one engineer's sessions draw on **their** plan limits; limits assume "ordinary, individual usage" | limited by org rate limits and budget only |
| LiteLLM budgets | a **usage meter** in API-list-price dollars (not money spent), still useful as a runaway brake | real dollars |
| Sharing | not allowed: no pooled or "service" subscription accounts | keys per team or workspace |
| Unattended / CI agents | not appropriate on personal plans | the right mode |
| Revocation | LiteLLM key revoke stops gateway access; the subscription token itself is revoked by the engineer (sign out) or the Claude admin | revoke at Console |
| Credential in sandbox | engineer's OAuth token (valuable, see threat model) | **none**: the key stays in LiteLLM |

**Recommendation for a real company:** API keys (or Bedrock / Vertex, or a Claude
Team / Enterprise plan) held only by the gateway. The sandbox then contains no
provider credential at all, budgets are real money, and unattended agents are
allowed. Subscription mode fits a pilot or individual engineers using their own
plan. The gateway config supports both, and switching modes needs no change to
the session template.

**Codex: not implemented in this demo** (not installed in devbox, no OpenAI
key for the exercise). Design: Codex's ChatGPT-subscription login talks to
`chatgpt.com` directly and would bypass the gateway, so Codex would run in
**API-key mode only** (`OPENAI_BASE_URL` → LiteLLM; config stub in `config.yaml`,
provider host `api.openai.com` already on the gateway allowlist).

## Per-session virtual keys (`session-key.sh`)

For platform-run sessions in `agent-sessions`. Developer namespaces use one key
per developer instead, issued at onboarding
([k8s/developers § LLM gateway key](../developers/README.md#llm-gateway-key)).

The virtual key is the only credential the *platform* hands to a session.

| Property | Setting | Threat addressed |
|---|---|---|
| Scope | models `claude-*` only | misuse for other models or providers |
| Network-bound | LiteLLM accepts connections only from `agent-sessions` (NetworkPolicy) | a stolen key is useless outside the cluster |
| Session budget | `$20` API-equivalent (`SESSION_BUDGET_USD`) | runaway loops |
| Engineer budget | `$50/day` across all their sessions (`USER_BUDGET_USD`) | many parallel sessions |
| Global budget | `$200/day` for the whole gateway (`config.yaml`) | platform-wide runaway |
| Rate | 60 req/min, 2M tokens/min, 4 parallel requests | tight loops, fan-out |
| Expiry | 8 h = max session lifetime | forgotten sessions, leaked keys |
| Attribution | `user_id`, `metadata.session` on every spend-log row | audit, chargeback |
| Delivery | `kubectl exec` stdin → `~/.claude/settings.json` (mode 600) | never stored in a Kubernetes object; a SandboxClaim `env` would be readable plaintext |
| Kill switch | `session-key.sh revoke`, or LiteLLM `/key/block` | stop a misbehaving session immediately |

## Hardening choices

- **Pinned images by digest:** `litellm-non_root:v1.106.6`. That is at least
  v1.105 (it has the "Client OAuth token" log filter) and older than two weeks at
  install time. Postgres is `17.10-alpine`.
- **Non-root image (uid 65534)**, Pod Security `restricted`, no SA token, all
  capabilities dropped.
- **Admin UI not exposed**: no Service of type LoadBalancer/NodePort, no Ingress.
  It is reached only with `kubectl port-forward` (see [Admin UI](#admin-ui)).
  Admin access = RBAC for `port-forward` in `llm-gateway` + the master key Secret.
- **Secrets generated at install** (`openssl rand`), never in git.
  `LITELLM_SALT_KEY` encrypts credentials stored in the DB.
- **Fail closed:** `allow_requests_on_db_unavailable: false`. Without the DB
  there are no budget checks, so there is no service.
- **No prompt bodies stored** (`store_prompts_in_spend_logs: false`). Spend logs
  keep metadata only: who, which session, model, tokens, cost.
- **No internet route:** egress is DNS, Postgres, and the proxy's gateway
  listener only. The model price map is bundled
  (`LITELLM_LOCAL_MODEL_COST_MAP`), not fetched from GitHub at startup.
- **Postgres:** ingress from LiteLLM only, no egress at all.

## Known limits

- **In subscription mode the gateway is the default path, not a hard boundary.**
  Claude Code's sign-in calls `api.anthropic.com` directly, so sessions must be
  able to reach that host. A malicious agent could therefore call the model API
  with the engineer's own token and skip LiteLLM's budgets and logs (it is still
  capped by the plan limits). API-key mode removes that host from the sessions
  allowlist. See [egress-proxy README](../egress-proxy/README.md#subscription-mode-and-apianthropiccom-accepted-trade-off).

- **OAuth token substitution (subscription mode).** LiteLLM forwards *whatever*
  OAuth token the client sends and does not check whose it is. A prompt-injected
  agent could send an attacker's token, and LiteLLM would deliver the
  conversation, including repository code, into the attacker's Anthropic account.
  The proxy cannot see this, because it is ordinary traffic to Anthropic.
  Mitigations: API-key mode (the gateway never forwards client credentials, so
  this path is closed), or a LiteLLM pre-call hook that pins the first-seen token
  hash per session key (design only). Detection: LiteLLM's "Client OAuth token"
  credential log filter.
- **Single replica, single Postgres:** demo scale. Production: HA LiteLLM, managed
  Postgres with backups, Redis for cross-replica rate limits.
- The `$` limits are API-list-price estimates in subscription mode, not the
  engineer's actual plan quota. Anthropic's plan limits remain the hard ceiling.
