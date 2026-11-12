# Egress proxy

The **only** workload in the platform with a network route to the internet.
Sessions and the LLM gateway reach the outside world only through it, and only
to allowlisted hostnames.

Implementation: **upstream Envoy** (`envoyproxy/envoy:v1.40.2`, pinned by digest),
configured declaratively in [`envoy.yaml`](envoy.yaml) and shipped as a ConfigMap
via kustomize. No custom code.

```shell
kubectl apply -k .
kubectl -n egress-proxy logs deploy/egress-proxy -f     # JSON access logs, two stages per connection
```

## Why it exists

NetworkPolicy filters by IP. Every Anthropic hostname (`api.anthropic.com`,
`platform.claude.com`, `claude.ai`, ...) resolves to the same address
(`160.79.104.10`), so an IP rule cannot allow "Claude sign-in" without also
allowing "call the model API directly". That would let a session bypass LiteLLM
and its budgets, logging and kill switch. Filtering by hostname needs an L7
component, and this proxy is it.

## Design

| Choice | Why |
|---|---|
| **Two listeners, two allowlists**: `:3128` sessions, `:3129` gateway | A session and the gateway need different destinations. NetworkPolicy decides who reaches which port (sessions → 3128 only, LiteLLM → 3129 only), so the allowlist cannot be chosen by the client. |
| Sessions allowlist = `platform.claude.com, claude.ai, claude.com` **+ `api.anthropic.com` (subscription mode only)** | What Claude Code needs to sign in and refresh a subscription token ([network requirements](https://code.claude.com/docs/en/network-config)). See "Subscription mode and api.anthropic.com" below. |
| Gateway allowlist = `api.anthropic.com, api.openai.com` | Provider APIs for LiteLLM. LiteLLM itself has no direct internet route. |
| **Stage 1: CONNECT** (HTTP connection manager) | Only `CONNECT <allowed-host>:443` is routed. Everything else (other hosts, IP literals, plain HTTP) matches the catch-all virtual host and gets **403**. |
| **Stage 2: TLS** (internal listener + `tls_inspector`) | The accepted tunnel goes to an Envoy *internal listener* that reads the TLS ClientHello. It forwards only if the **SNI is allowlisted** and resolves the upstream **from that SNI** (`sni_dynamic_forward_proxy`). Otherwise the connection is dropped (`deny-sni`). This stops "CONNECT claude.ai, then TLS to api.anthropic.com": both share one IP, so without this check a session could bypass the gateway (tested). |
| Proxy does the DNS resolution | Clients only need to resolve cluster services. |
| JSON access logs for both stages (target, SNI, status, flags, bytes up/down, duration) | Audit trail, plus data for exfiltration alerts such as large `bytes_up` to a sign-in host. |
| Connection cap (overload manager, 1000) | A session cannot exhaust the proxy. |
| Upstream Envoy, digest-pinned, non-root (uid 101), read-only root FS, no capabilities, admin interface on localhost only | Standard, widely deployed component; config is declarative and reviewable. |

### Why Envoy, and the alternatives considered

- **Envoy (chosen):** declarative config and a pinned upstream image. It is a
  drop-in for the `HTTPS_PROXY` contract, so nothing else in the architecture
  changes. Cost: the allowlist appears twice per listener (CONNECT `domains` and
  SNI `server_names`), and both must be edited together.
- **Cilium FQDN policy (production target):** egress becomes
  `CiliumNetworkPolicy` CRDs per namespace (`toFQDNs` plus SNI `serverNames`),
  with no proxy service and no reliance on clients honouring `HTTPS_PROXY`. It
  also closes the DNS-exfiltration gap, because Cilium's DNS proxy can restrict
  which names a pod resolves. Not done here because it means replacing k3s's
  flannel and kube-router networking, which requires reinstalling cluster
  networking. Name-only rules would not be enough either: all Anthropic hosts
  share one IP, so SNI rules (which Cilium enforces with an embedded Envoy) are
  still needed.
- **Squid:** works (`dstdomain` ACLs plus SSL peek on SNI), but its config is
  harder to review, and it adds nothing over Envoy here.
- The first prototype was a ~150-line Python CONNECT proxy implementing the same
  contract. It was replaced by Envoy to avoid custom code on the security path.
  The tests did not change and all pass against Envoy.

## Subscription mode and `api.anthropic.com` (accepted trade-off)

The first design left `api.anthropic.com` off the sessions allowlist, so that
model traffic could only go through LiteLLM. In the `/login` test Claude Code
failed at start-up with `Failed to connect to api.anthropic.com:
ERR_PROXY_TUNNEL`. The proxy log showed a `403` for `api.anthropic.com` next to
an allowed `platform.claude.com`. Claude Code's start-up connectivity check and
its subscription sign-in (account profile after the token exchange) call
`api.anthropic.com` **directly**: they do not use `ANTHROPIC_BASE_URL`.

That host is also the model API, and the proxy can't tell the sign-in calls from
`/v1/messages` without decrypting TLS. Options considered:

| Option | Verdict |
|---|---|
| **Allow `api.anthropic.com` for sessions in subscription mode** | **chosen** |
| TLS inspection of `api.anthropic.com` with a platform CA, allowing only sign-in paths | rejected: the platform would see engineers' OAuth tokens in cleartext (Anthropic: platforms "may not collect, store, or intermediate Claude.ai credentials"), and it adds a CA private key to protect |
| Keep it blocked and drop the subscription demo | rejected: no end-to-end agent run without an API key |

**Consequence:** in subscription mode the gateway is the *default* path, not a
hard boundary. Claude Code sends model traffic to LiteLLM because managed
settings pin `ANTHROPIC_BASE_URL`, and a repository cannot override that. But
an agent that deliberately calls `api.anthropic.com` with the engineer's own token
skips LiteLLM's budgets and logs. It is still capped by the engineer's plan
limits, and it still cannot reach any non-Anthropic host.

**API-key mode closes this.** Sessions don't sign in, so `api.anthropic.com` is
removed from the sessions allowlist (one line in `envoy.yaml`, both places), and
the gateway becomes a hard boundary again. The allowlist is therefore
**mode-dependent**. In production it would be generated from the auth mode, not
edited by hand.

## Known limits

- **Clients must use the proxy.** Sessions have no other route (NetworkPolicy),
  so a client that ignores `HTTPS_PROXY` simply fails. It cannot bypass.
- **No TLS interception**, so the HTTP `Host` header inside the tunnel is not
  checked. Domain fronting (SNI `claude.ai`, Host `api.anthropic.com`) depends on
  Anthropic's edge rejecting the mismatch, which is not verified here. Closing it
  means TLS interception for the sign-in hosts, which in turn requires trusting a
  platform CA inside sessions.
- **The sign-in hosts are a data channel.** A compromised session can send data
  to `claude.ai`, which is still Anthropic, not an attacker. Byte counters make
  large uploads visible.
- Allowlists are global per listener. Per-team or per-repo allowlists (git
  hosts, package mirrors) would add listeners or move to identity-aware egress
  (e.g. per-namespace Cilium policy).

## Tests

`../llm-gateway/test/verify.sh` section C. From inside a real session: sign-in
hosts allowed (incl. `api.anthropic.com`, subscription mode); `api.openai.com`
(gateway-only), `github.com`, `registry.npmjs.org`, plain HTTP, IP literal, an SNI
not on the sessions allowlist, and direct internet without the proxy are all denied;
the gateway listener (:3129) is unreachable from sessions. The access log shows
which stage denied each one:

```
{"stage":"connect","target":"github.com:443","status":403, ...}            # not allowlisted
{"stage":"connect","target":"claude.ai:443","status":200, ...}             # CONNECT ok ...
{"stage":"tls","decision":"deny-sni","sni":"api.openai.com", ...}          # ... but SNI rejected
{"stage":"tls","decision":"allow","sni":"platform.claude.com","upstream":"160.79.104.10:443", ...}
```
