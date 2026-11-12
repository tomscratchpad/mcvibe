# Developer guide: remote agent sandboxes

Run Claude Code in disposable, isolated sandboxes on the platform cluster, and
watch and steer them from the **Herdr desktop app**: several in parallel, still
running when your laptop sleeps, reachable from another computer.

```
your laptop                                        platform cluster
───────────                                        ────────────────
Herdr app ── herdr-core (hc) ──┐
                               ├─ sbx exec ── Kubernetes API ──▶ your namespace sbx-<you>
sbx (create/list/delete) ──────┘   (your identity,              └ sandbox (gVisor): Claude Code,
                                    RBAC, audit)                   herdr-core, your repo
```

## What you need

| Tool | Install | What for |
|---|---|---|
| **Herdr desktop app** | `curl -fsSL https://releases.herdr.dev/app/install.sh \| sh`, then open **Herdr** from your apps (or run `herdr-app`). macOS: `brew install --cask herdr-app`. Windows: `irm https://releases.herdr.dev/app/install.ps1 \| iex` | See and drive your sandboxes. Linux needs a desktop session and a working Vulkan driver |
| **herdr-core** (`hc`) | installed by the app installer if missing; the app keeps it updated | the app's engine; `sbx` registers sandboxes in it |
| **sbx** | platform release (prototype: `cd cli && go build -o bin/sbx ./cmd/sbx`); put it on your PATH and keep it there | create, list and delete sandboxes; the transport the app uses |
| Cluster access | prototype: a kubeconfig. Production: `sbx` signs you in with company SSO (OIDC, not built yet) | your identity for everything |

Notes:
- Add `~/.local/bin` to your PATH if the installer asks.
- In the Herdr app, check **Settings → Privacy**: usage telemetry is on by
  default in releases. Follow company policy.
- Do not move or delete the `sbx` binary after creating sandboxes: the app
  reaches each sandbox by running it. If you do, run `sbx connect <name>` again.

## One-time onboarding

A platform admin creates your namespace (`sbx-<you>`) with
`k8s/developers/developer.sh add <your-username>`, through a reviewed PR. Check it:

```shell
sbx whoami
# user:      alice@corp.com
# namespace: sbx-alice
```

## Daily workflow

**1. Create a sandbox from your repo** (one per task or branch; cold start takes a few seconds):

```shell
cd ~/src/my-app
sbx create fix-login            # --ttl 8h by default: it is deleted automatically after that
```

Your repo's **committed** history is copied to `/workspace/repo` in the
sandbox, on branch `agent/fix-login`. Uncommitted files (like `.env`) are not
copied. The sandbox has no GitHub access and no git credentials. It appears as
**`sbx-fix-login`** in the Herdr app's sidebar (Machines). Start agents in
`/workspace/repo`.

**2. Start an agent.** In the app, open a **space** on `sbx-fix-login` and start
a terminal there: it starts Claude Code in `/workspace/repo` (exit Claude to get
a shell in the same terminal; `SBX_NO_CLAUDE=1` turns this off). Terminals run
*inside the sandbox*, not on your laptop. From a terminal instead:

```shell
hc run --on sbx-fix-login --bg claude "Fix the login redirect bug"
```

The first time in a sandbox, run `/login` in Claude Code and finish Anthropic's
sign-in in your own browser. Your subscription token stays in your sandbox;
the platform never stores it.

**3. Run several in parallel.** Create more sandboxes (your quota allows 2 at a
time). The app's agents sidebar shows every agent across machines, with
Working, Blocked (needs you) and Done states.

**4. Follow up, attach, stop.** Click an agent to see it and type follow-up
prompts. Close the window or let the laptop sleep: everything keeps running in
the sandbox, and the app reconnects when you come back.

**5. From another computer.** Install the app and `sbx` there, then:

```shell
sbx list                  # your sandboxes, whichever computer created them
sbx connect fix-login     # now in this computer's Herdr app too
```

**6. Without the app** (scripts, quick checks):

```shell
sbx shell fix-login                       # interactive bash in the sandbox
sbx exec fix-login -- git status          # one command, exit code preserved
```

**7. Get the agent's work back.** Any time, from your repo:

```shell
sbx pull fix-login              # → local branch sbx/fix-login (agent commits + a WIP commit of uncommitted work)
git log -p HEAD..sbx/fix-login  # review
git merge sbx/fix-login         # or rebase / cherry-pick; then push to GitHub yourself
```

This works like `git fetch`: your working tree and current branch are never
touched, so you can pull while you keep working. Conflicts, if any, appear
when you merge, as with any branch. Nothing from the sandbox (git hooks,
config) is copied to your laptop.

**8. Finish.**

```shell
sbx delete fix-login      # pulls one last time into sbx/fix-login, then destroys the sandbox
sbx prune                 # removes app entries for sandboxes that expired (TTL)
```

Run `delete` from your repo so the last pull lands there. If the pull fails, the
sandbox is kept; `sbx delete --no-pull` discards its work. A sandbox that
expired (TTL) is gone with its work: pull before the deadline.

## What a sandbox can and cannot do

| | |
|---|---|
| ✅ | Run Claude Code with the platform's managed settings; use git, node, python and ripgrep in `/workspace` |
| ✅ | Reach the model through the company LLM gateway (your key: $50/day, rate limits) |
| ⚠️ | Internet: only Anthropic sign-in, through the egress proxy. GitHub, npm and everything else are blocked: code comes in and out with `sbx` |
| ❌ | Reach the Kubernetes API, other sandboxes, the node or cloud metadata |
| ❌ | Be reached from the network: there is no SSH and no open port. Only you, through `sbx` and your cluster identity |
| ❌ | Survive its TTL: there is no persistent disk. `sbx pull` before it expires (`sbx delete` pulls for you) |

Your colleagues cannot open, list or delete your sandboxes, and you cannot
open theirs. This is enforced by Kubernetes RBAC *and* admission policies
([k8s/developers/README.md](../k8s/developers/README.md)).

Treat everything an agent reads (repos, issues, docs, tool output) as
untrusted: it may contain prompt injection. The sandbox limits what a
manipulated agent can reach. Review agent-written code before merging, as you
would a colleague's.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `sbx-<name>` missing from the app | `sbx connect <name>`. Created with `--hc-temp`? Temp machines are hidden in the app |
| Machine shows "connecting" / error | `sbx list`: is it Ready? Expired (TTL)? Then `sbx prune`. Is `sbx` still at the same path? |
| `forbidden` / `belongs to another developer` | Wrong identity or namespace: `sbx whoami`. Not onboarded yet: ask the platform team |
| `namespaces "sbx-<you>" not found` | Your identity is not onboarded (`sbx whoami`). With the VM's admin kubeconfig you are `system:admin`, hence `sbx-admin`: use `sbx --as alice@corp.com …` for the demo, or onboard with `k8s/developers/developer.sh add <user>` |
| `not in a git repository: no code copied` | Run `sbx create` from your repo, or `sbx push <name>` from it afterwards |
| `/workspace/repo already exists` on push | Code was already copied. Use a new sandbox for another repo |
| `sbx create` fails with `exceeded quota` | 2 sandboxes per developer: `sbx delete` one |
| Claude Code cannot reach the model | Run `/login` once per sandbox. `Missing Anthropic API Key` means not signed in yet. Sandboxes created before your gateway key was issued: recreate them |

## Current limitations (prototype)

- **Claude Code only.** Codex is not implemented in this demo (design: API-key
  mode through the gateway, see `k8s/llm-gateway/README.md`).
- **Gateway key:** one key per developer (issued at onboarding, $50/day), not
  one per sandbox. Your sandboxes share that daily budget.
- **Sign-in:** `--auth oidc` (company SSO) is a placeholder. The prototype uses
  an admin kubeconfig and impersonation (`--as`).
- **Mobile:** not tested. The Herdr phone apps cannot run `sbx`, so they
  probably cannot reach sandboxes directly.
- **SSH tools:** VS Code Remote-SSH and plain `ssh`/`rsync` do not work, by design.
