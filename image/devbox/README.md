# devbox image

The image every agent sandbox runs: the `claude-code` SandboxTemplate in every
developer namespace (`k8s/developers/base`) and in `agent-sessions`
(`k8s/policies/40-…`, plus the Kata example). Created by `sbx create`, and
controlled remotely from the Herdr desktop app (herdr-core) over the Kubernetes
exec API.
`image/testing` stays as the minimal smoke-test image.

## Build and publish

```shell
cd image/devbox
sg docker -c 'docker buildx build . -t localhost:5000/devbox:2 --load'   # or: newgrp docker
sg docker -c 'docker push localhost:5000/devbox:2'
```

Tags are immutable: every change gets a new tag, and templates are bumped
explicitly. Running sandboxes keep their image until they are recreated.

| Tag | Digest | Change |
|---|---|---|
| `devbox:2` (current) | `sha256:b94fe3702e6d0e1427d2599b522daec75b8861683faf65f144acf50d8fbb9465` | + rtk 1.1.0 |
| `devbox:1` | `sha256:178b820316a92261a2baaae7354cbbfd0f7812465edbf6399fcc8f10f3c4b1b4` | initial: Claude Code, herdr-core |
The pod VAP allows any `localhost:5000/` image.

## Contents

| Component | Version / pin | Why it is there |
|---|---|---|
| agent-sandbox `python-runtime-sandbox` base | digest `sha256:e78ef64f…` | Runtime API on :8888 (Python SDK, Sandbox Router), Debian trixie, Python |
| git, ripgrep | Debian trixie | Claude Code shells out to them |
| Node.js | 24.22.3, official tarball, SHA-256 verified | Claude Code runtime. Replaces NodeSource's `curl \| bash` used in `testing` |
| Claude Code | `@anthropic-ai/claude-code@2.1.322` | The agent |
| **herdr-core** (`herdr-core`, `hc` symlink) | 0.1.20, `herdr-core-linux-x86_64.tar.gz`, SHA-256 from the release manifest | Remote end of herdr-core machines: `herdr-core connect` / `serve`, persistent terminals |
| **rtk** (`rtk-oss`) | 1.1.0, `rtk-oss-x86_64-unknown-linux-musl.tar.gz`, SHA-256 from the release `checksums.txt` | Token savings: compresses the output of the agent's shell commands. See below |
| user `python` (UID 1000) | | The only user. The build fails if UID 1000 has another name |

**Deliberately absent:** openssh-server/client and any SSH keys. Remote access
is the Kubernetes exec API only (see `cli/README.md`). Also absent: `ps`
(procps), sudo and compilers beyond what the base image ships.

## Why herdr-core

Developers control sandboxes from the **Herdr desktop app**. The app is a
window onto herdr-core: it shows every machine in the local core's list
(`hc machine ls`) and runs its spaces' terminals in that machine's core.
`sbx create` adds each sandbox to that list with
`hc machine add --via 'sbx exec <name> -- sh -c' --name sbx-<name>`, so the
remote end the image must provide is `herdr-core` on PATH (`--via` runs
`herdr-core connect` there, and fails cleanly if it is missing). Nothing else
from Herdr is needed in the sandbox: the app runs only on the laptop.

Sandboxes are registered *without* `--temp`: the app hides `--temp` machines.

Why baked in rather than installed at runtime: sandboxes have no internet
egress, and `hc machine add --install` would copy the *laptop's* binary into the
sandbox, which is unpinned and unreviewed. `sbx` never passes `--install`.

Verified on the cluster (gVisor): `hc machine add --via …` connects; `hc run`
starts a terminal; `herdr-core serve` (PID 10) and the terminal survive the
exec stream closing, and a new connection reads the same terminal.

## rtk: token savings

[rtk](https://github.com/rtk-ai/rtk) is a Claude Code `PreToolUse` hook on `Bash`.
It rewrites shell commands to their rtk form (`git status` → `rtk git status`),
so the model receives condensed output (status lines, failing tests, lint
errors) instead of the full output. Built-in Read/Grep/Glob are untouched. In a
fresh repo, `git status` went from 99 to 55 bytes. The savings grow with
verbose tools (test runners, builds, `git log`).

| Choice | Why |
|---|---|
| **`rtk-oss` edition, v1.1.0** | Apache-2.0 only. The default `rtk` binary also links the BUSL-licensed engine, whose optional pipelines can send content to models. 1.1.0 (2026-10-25) respects the platform's two-week minimum release age; 1.2.0 was 4 days old |
| Pinned by **SHA-256** (`ADD --checksum`) | Same supply-chain rule as Node and herdr-core. Not the `curl … \| sh` installer |
| Hook in **managed settings** (`k8s/…/claude-code-managed-settings`), not `rtk init` | Platform-owned: a repository's `.claude/settings.json` cannot remove or replace it. `rtk init -g` would write `~/.claude/settings.json`, which is the user layer |
| Hook command `if command -v rtk …; then exec rtk hook claude; fi` | A no-op on images without rtk, so the shared ConfigMap does not break running `devbox:1` sandboxes |
| rtk's model instructions as a **managed `CLAUDE.md`** (`/etc/claude-code/CLAUDE.md`, same ConfigMap) | Loaded in every session. It tells the model that output is condensed and how to use `rtk bypass <cmd>` when it needs the raw output |
| `RTK_TELEMETRY_DISABLED=1` | Telemetry is opt-in already. Belt and braces, and the egress proxy blocks `telemetry.rtk-ai.app` anyway |
| `RTK_SUPPRESS_HOOK_WARNING=1` | rtk only detects hooks installed via `rtk init`. Without this, every command's output starts with "No hook installed — run `rtk init -g`", which wastes tokens and nudges the agent to reconfigure itself |
| `RTK_TRUST_PROJECT_FILTERS` **unset** | Project-level filter files come from the (untrusted) repository. A malicious repo could ship filters that hide failing tests or suspicious output from the agent and the reviewer |

**Interaction with permissions:** the hook returns an `updatedInput`, so
permission rules see `rtk git status`, not `git status`. The managed deny rules
are written for that (`Bash(*git push*)`), see
[k8s/policies § Permission modes](../../k8s/policies/README.md#permission-modes-claude-code).

Verified on the cluster (gVisor, fresh `devbox:2` sandbox): `rtk 1.1.0` on PATH,
`/etc/claude-code/CLAUDE.md` mounted, the managed hook command returns
`{"updatedInput":{"command":"rtk git status"}}`, and there is no warning banner.

## Security notes

- **Supply chain.** Base by digest. Node, herdr-core and Claude Code are pinned
  by checksum or exact version (Docker `ADD --checksum` for herdr-core). Apt
  packages float within Debian trixie security updates. Next steps: sign the
  image (cosign) and enforce the signature plus a digest at admission (the VAP
  checks only the registry prefix today); verify Node's `SHASUMS256.txt` GPG
  signature instead of a hash copied from one download; read herdr-core's
  checksum from a signed manifest.
- **No new network surface.** The image listens on nothing new; NetworkPolicy
  (DNS, LiteLLM, egress proxy), PSA `restricted` and gVisor are unchanged.
- **herdr-core runs as UID 1000**, like the agent. A compromised agent can drive
  its own herdr-core (start terminals, read their screens). It already has those
  rights in its own sandbox and gains nothing outside it. The developer's hc
  talks to it only through the developer's own authenticated exec stream.
- **Same UID for agent and control plane.** Separating them (herdr-core as a
  sidecar with another UID) would stop an agent from tampering with the terminals
  the developer watches. It is not done here; listed as future work.
