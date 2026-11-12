# syntax=docker/dockerfile:1.10
#
# devbox: the image developers' sandboxes run (k8s/developers SandboxTemplate).
# Contents and rationale: README.md next to this file.
#
# Every external input is pinned by digest or checksum: the build fails
# rather than silently picking up a changed artifact.

# agent-sandbox runtime base (uvicorn runtime API on :8888, kept for the
# Python SDK / Sandbox Router path). Pinned by digest; bump deliberately.
ARG BASE=us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/python-runtime-sandbox@sha256:e78ef64f9eae21c2d49177cd77dbc3a799f7521f742850e4ee27fe444301c303
FROM ${BASE}

ARG NODE_VERSION=24.22.3
ARG NODE_SHA256=4f2b175a6d53a5c617a80dba2676238a0688cef28c6da4eba35c7d844e66fcfb
ARG CLAUDE_CODE_VERSION=2.1.322
ARG HERDR_CORE_VERSION=0.1.20
ARG HERDR_CORE_SHA256=ac6b86890c451a37584620aaa438b71bc0d0bd430a2e7c1d7e095f052c414e74
# rtk (token-saving output filter for agent shell commands). OSS edition:
# Apache-2.0 only, without the BUSL engine and its model-backed pipelines.
# Checksum = checksums.txt of the GitHub release.
ARG RTK_VERSION=1.1.0
ARG RTK_SHA256=4dec94a64035e08189ae82fd5408fe4ae9ae27a665fc07e3e3ec3c4c2d8e8a3e

USER root

# OS packages (Debian trixie): git and ripgrep for Claude Code; xz-utils to
# unpack Node. Deliberately NO openssh-server: remote access is the
# Kubernetes exec API only.
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git ripgrep xz-utils \
  && rm -rf /var/lib/apt/lists/*

# Node from the official tarball with the published SHA-256, instead of
# NodeSource's `curl | bash` apt setup.
RUN curl -fsSLo /tmp/node.tar.xz \
       "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
  && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
  && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner \
       --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE' \
  && rm /tmp/node.tar.xz \
  && node --version

RUN npm install -g --no-fund --no-audit "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
  && npm cache clean --force

# herdr-core: the remote end of herdr-core machines (`hc machine add --via`).
# The developer's hc reaches it with `sbx exec <name> -- sh -c '... herdr-core
# connect'`, which looks it up on PATH. Sandboxes have no internet egress
# and sbx never lets hc copy a binary in, so it is baked here. Checksum =
# releases.herdr.dev/core/<version>/manifest.json.
ADD --checksum=sha256:${HERDR_CORE_SHA256} \
    https://releases.herdr.dev/core/${HERDR_CORE_VERSION}/herdr-core-linux-x86_64.tar.gz /tmp/hc.tar.gz
RUN tar -xzf /tmp/hc.tar.gz -C /usr/local/bin --no-same-owner herdr-core \
  && chmod 0755 /usr/local/bin/herdr-core \
  && ln -s herdr-core /usr/local/bin/hc \
  && rm /tmp/hc.tar.gz \
  && herdr-core --version

# rtk: compresses the output of the agent's shell commands (git, test runners,
# linters...) before it reaches the model. Wired in by the platform's managed
# Claude Code settings (PreToolUse hook `rtk hook claude`), not by `rtk init`,
# so a repository cannot remove it. Telemetry is opt-in and also disabled via
# RTK_TELEMETRY_DISABLED in managed settings.
ADD --checksum=sha256:${RTK_SHA256} \
    https://github.com/rtk-ai/rtk/releases/download/v${RTK_VERSION}/rtk-oss-x86_64-unknown-linux-musl.tar.gz /tmp/rtk.tar.gz
RUN tar -xzf /tmp/rtk.tar.gz -C /usr/local/bin --no-same-owner rtk \
  && chmod 0755 /usr/local/bin/rtk \
  && rm /tmp/rtk.tar.gz \
  && rtk --version

# Sandbox user: UID 1000, named "python". Fail the build if the base image
# ever ships UID 1000 under another name.
RUN getent passwd 1000 >/dev/null || useradd -m -u 1000 python; \
  test "$(getent passwd 1000 | cut -d: -f1)" = python \
  && mkdir -p /workspace /home/python/.claude \
  && chown 1000:1000 /home/python \
  && chown -R 1000:1000 /workspace /home/python/.claude \
  && chmod 700 /home/python

# The base image's CMD is "uvicorn main:app ..." which imports main from /app;
# WORKDIR moves to /workspace for agent work, so keep /app importable.
ENV PYTHONPATH=/app
ENV HOME=/home/python
WORKDIR /workspace

USER 1000
