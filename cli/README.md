# sbx: developer CLI for agent sandboxes

One static Go binary, distributed to every developer. It creates and destroys
disposable sandboxes (agent-sandbox `SandboxClaim`s) in the developer's own
namespace, and registers them as **herdr-core** machines, so they appear in
the developer's **Herdr desktop app** for remote control.
Every interaction uses the **Kubernetes exec API** (what `kubectl exec` uses):
there is no SSH anywhere.

```shell
sbx whoami                         # identity the cluster sees + your namespace (sbx-<you>)
sbx create feature-x [--ttl 8h] [--template claude-code] [--no-hc] [--no-push]
                                   # run it from your git repo: the code is copied to /workspace/repo
sbx connect feature-x              # show an existing sandbox in this computer's Herdr app
sbx list                           # status, hc machine state, expiry
sbx shell feature-x                # interactive bash (TTY)
sbx exec [-t] feature-x -- cmd...  # run a command; stdio + exit code passthrough
sbx pull feature-x                 # fetch the agent's work into local branch sbx/feature-x
sbx push feature-x                 # copy the repo in later (if create ran outside it)
sbx delete feature-x [--no-pull]   # pull, hc unregister, destroy (waits until the pod is gone)
sbx prune                          # forget hc machines of sandboxes reaped by their TTL

# then, in the Herdr app: sidebar → sbx-feature-x → new space → start Claude
hc run --on sbx-feature-x --bg claude    # same from a terminal (herdr-core CLI)
```

Build: `go build -o bin/sbx ./cmd/sbx` (Go 1.25+, client-go v0.37; on the demo VM Go 1.27.4 is in `~/.local/go`).
Developer setup and daily workflow: [docs/developer-guide.md](../docs/developer-guide.md).

## Remote access design: Kubernetes exec, no SSH

```
developer laptop                                   cluster
───────────────                                    ───────
Herdr app ─▶ local herdr-core (hc)
                      │
sbx shell / sbx exec ─┤
                      ├─ k8s exec (WebSocket, SPDY fallback; TLS) ─▶ API server ─▶ kubelet ─▶ sandbox pod (gVisor)
hc ──via "sbx exec <name> -- sh -c '… herdr-core connect'"─┘       │                          herdr-core serve
                                                                   ├ authn (kubeconfig → OIDC)  └ terminals: claude, shells
                                                                   ├ RBAC: own namespace only
                                                                   ├ VAP developer-exec-owner
                                                                   └ audit log
```

- **Transport.** `sbx exec` streams stdin, stdout and stderr over the exec API
  and exits with the remote command's exit code. stdout carries nothing but the
  command's output, so it is a clean byte pipe.
- **herdr-core.** `hc machine add --via <command>` reaches a machine through any
  such pipe; hc appends its remote line (`sh -c '… exec herdr-core connect'`).
  `sbx create` registers
  `--via "'<sbx>' --namespace sbx-<you> exec <name> -- sh -c" --name sbx-<name>`.
  Not `--temp` by default: the Herdr desktop app hides `--temp` machines, so
  sandboxes would be invisible there (`--hc-temp` opts in for agent-only use).
  The hc profile stores only that command line, never credentials: every
  (re)connect re-authenticates to the API server.
- **Persistence.** `herdr-core serve` and its terminals keep running in the
  sandbox when the exec stream drops (laptop sleeps, network changes). hc
  reconnects through the same command. Verified on gVisor.

Why not SSH (the previous design: `ProxyCommand` + `sshd -i` over exec):

| | SSH over exec (dropped) | exec only (current) |
|---|---|---|
| Credentials to manage | k8s identity **plus** a per-developer SSH key, a per-sandbox host key and pinned known_hosts | k8s identity only |
| In the image | openssh-server, sshd config, keys generated at create | nothing extra: herdr-core only |
| Revocation | IdP **and** authorized_keys | IdP / RBAC only |
| Admission control | exec into the pod, then an opaque SSH session | every session is a `pods/exec` CONNECT that the owner VAP checks |
| Client compatibility | any OpenSSH tool (VS Code Remote-SSH, rsync) | the Herdr desktop app, `hc`, `sbx` and kubectl; no plain `ssh` |

SSH only added a second credential system on top of the exec path it already
rode on. Its only advantage was compatibility with SSH-based tools, which we
give up deliberately: the supported client is the **Herdr desktop app**, which
works with any herdr-core machine, including `--via` ones. See
[image/devbox/README.md](../image/devbox/README.md#why-herdr-core).

Other options that were rejected: an sshd/Service/LoadBalancer listener (an
inbound path into the sandbox; quota forbids LB/NodePort), `kubectl port-forward`
(still needs a listener; also an extra verb to grant), and the Sandbox Router
(HTTP to the runtime API, not a terminal transport).

Trade-offs:
- The API server and kubelet are in the data path. That is fine for terminals and
  agent control, but not for bulk transfer. Code moves as git bundles (`sbx push/pull`), fine for normal repos.
- Exec streams end on API server restarts. hc reconnects, and nothing in the
  sandbox is lost.
- An established stream survives token revocation until it drops (see
  [k8s/developers/README.md](../k8s/developers/README.md#remaining-risks-and-next-steps)).

## What `create` does

1. Authenticates (`--auth`, see below) and asks the API server who the caller
   is (`SelfSubjectReview`). The namespace (`sbx-<slug>`) and the owner
   annotation derive from that answer, not from local config.
2. Creates `SandboxClaim <name>` in that namespace against the platform warm
   pool `claude-code`, with annotation `agent-platform/owner-username=<you>`
   (checked by admission). Developers never send a pod spec.
   `lifecycle.shutdownTime = now + ttl`, `shutdownPolicy: Delete`: a hard
   expiry enforced by the controller (runaway spend, forgotten sandboxes).
3. Waits for Ready and resolves the pod via `Sandbox.status.selector`. It fails
   immediately on terminal controller errors (`Invalid*`).
4. If run inside a git repo, copies its committed history into
   `/workspace/repo` (see [Code in and out](#code-in-and-out-demo-push--pull-over-exec)).
5. Registers the herdr-core machine `sbx-<name>`. It never passes
   `--install`, so sbx never lets hc copy a binary from the laptop into a sandbox.
   The image ships a pinned herdr-core.

`delete` first pulls the sandbox's work into branch `sbx/<name>` (and stops if
that fails), then forgets the hc machine (it also works if the sandbox already
expired), then deletes the claim with foreground propagation and waits until
the pod is gone. `prune` removes hc machines of this namespace whose sandbox no
longer exists.

## Isolation between developers

One namespace per developer, RBAC bound in that namespace only, and owner
admission policies on claims and `pods/exec`. Setup, policies and the
26-check test suite are in [k8s/developers/README.md](../k8s/developers/README.md).

## Kubernetes authentication (placeholder)

`internal/auth` defines a `Provider` interface; everything else consumes only a
`rest.Config`.

- `--auth kubeconfig` (default, **demo only**): standard kubeconfig loading. On
  the prototype this is the k3s admin certificate. Hidden flags
  `--as <user> [--as-group g]` (or `SBX_AS`) impersonate a developer, so the demo
  exercises real per-developer RBAC and admission without an IdP. Impersonation
  needs the `impersonate` verb, which developers never get.
- `--auth oidc` (**placeholder, returns "not implemented"**): device-code or PKCE
  login against the company IdP, refresh token in the OS keychain, API server
  configured with structured OIDC authentication (username claim → e.g.
  `oidc:alice@corp.com`, which must match the namespace owner annotation), and a
  pinned cluster CA.

## Code in and out (demo: `push` / `pull` over exec)

Sandboxes cannot reach GitHub (that would open an exfiltration path: the proxy
only sees the hostname, so "github.com" means every repo, gists and raw
downloads). For the demo, code moves through `sbx` and the engineer's laptop:

- **In** (`sbx create` from inside a git repo, or `sbx push`): the committed
  history of `HEAD` is sent as a `git bundle` over the exec API and cloned into
  `/workspace/repo` on branch `agent/<name>`. Uncommitted files (e.g. `.env`)
  stay on the laptop. The sandbox gets no remote and no credentials.
- **Out** (`sbx pull`, and automatically on `sbx delete`): the sandbox's branch,
  plus a WIP commit of uncommitted work (made without touching its files), comes
  back as a bundle and is fetched into local branch **`sbx/<name>`**.
  - Like `git fetch`: the working tree and current branch are never touched, so a
    pull never conflicts. Review and merge with plain git
    (`git log -p HEAD..sbx/<name>`, `git merge` or `git rebase`), and resolve any
    conflicts then, as with a colleague's branch.
  - Only commits and refs cross over, never hooks or git config (tested with a
    planted `post-merge` hook). Nothing from the sandbox runs on the laptop.
  - `delete` refuses to destroy a sandbox whose work could not be pulled
    (`--no-pull` discards it). Run commands from the repo (or pass `--repo`).
- **Not handled** (demo scope): one repo per sandbox, at `/workspace/repo`; no
  submodules or LFS; a full `git add -A` snapshot on each pull (fine for normal
  repos, slow for huge ones).

**Production design: an L7 git gateway.** An in-cluster Envoy that sandboxes
reach over plain HTTP (`url.<gateway>.insteadOf https://github.com/`). It
forwards only `/<org>/<repo>.git/` paths of allowlisted repos to GitHub, adds a
short-lived GitHub App token itself (none in the sandbox), allows push only
where configured, and logs every fetch and push per sandbox. Pushes land on
`agent/*` branches (GitHub rulesets) and merge only through a reviewed PR. This
removes the laptop from the loop for long unattended runs.

Test: `test/code.sh` (17 checks, real cluster, as `alice@corp.com`):

```
== create = sandbox + push                         4 PASS  (code on agent/codetest, no remote, .env not copied)
== agent works; engineer keeps working locally
== pull (like git fetch: nothing local is touched) 7 PASS  (agent + WIP commits, branch/edits untouched, hooks not copied)
== review and merge with plain git                 4 PASS
== delete = pull + destroy                         2 PASS
== 17 passed, 0 failed
```

## Status: tested vs not

| | Status |
|---|---|
| Unit tests (`go test ./...`): owner slug, name rules | ✅ |
| `sbx create` → Ready (gVisor, devbox image) in about 3-8 s, hc machine registered | ✅ e2e on the demo cluster, as impersonated `alice@corp.com` |
| `sbx exec` stdin, stdout and exit-code passthrough; `sbx shell` interactive bash, exit code | ✅ |
| hc over exec: `hc run --on sbx-demo`, `hc read`; terminal and `herdr-core serve` survive the exec stream closing; reconnect reads the same terminal | ✅ |
| `sbx delete` (hc unregister, pod gone), `sbx prune` after out-of-band deletion | ✅ |
| Developer isolation (RBAC + VAP, including a misconfigured RoleBinding) | ✅ 26/26, `k8s/developers/test/verify.sh` |
| Code in/out: `create` pushes, `pull`, `delete` pulls then destroys | ✅ 17/17, `test/code.sh` |
| `--auth oidc` | placeholder |
| Claude Code talking to LiteLLM from a developer sandbox | ✅ per-developer gateway key injected by the template (k8s/developers README, "LLM gateway key"); the key passes gateway auth and spend is attributed to the developer and the sandbox. The model call itself needs `/login` (subscription mode) |
| `tty` inside `sbx shell` on gVisor reports "not a tty" although bash runs interactively | cosmetic, not investigated |
