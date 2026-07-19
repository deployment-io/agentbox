# ---- Stage 1: Build agentbox binary ----
FROM golang:1.24-bookworm AS builder

WORKDIR /src
COPY . .

# AGENTBOX_VERSION stamps the release tag into the binary (logged at
# startup so session logs attribute every run to an exact release).
# The release workflow passes the tag; local builds default to "dev".
ARG AGENTBOX_VERSION=dev

# Once the module acquires external dependencies, split this into
# `COPY go.mod go.sum ./ && RUN go mod download && COPY . .` so the
# dep-download layer can cache independently of source changes.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${AGENTBOX_VERSION}" -o /out/agentbox ./cmd/agentbox


# ---- Stage 2: Runtime ----
FROM debian:bookworm-slim

# Language runtimes and build tools needed by supported agents.
# Node 22 + yarn + pnpm: Claude Code, Codex, and opencode are all npm-packaged
# (@openai/codex requires Node >= 22; opencode ships via the opencode-ai wrapper,
# which fetches its binary on install), plus package-manager-agnostic JS/TS
# dependency vendoring + verify (npm / yarn / pnpm). These install to the
# system prefix (before NPM_CONFIG_PREFIX is set below), so they survive the
# runtime tmpfs mounted over /home/agent.
# Python: Aider and future pip-packaged agents (v2+).
# build-essential, git, curl: used by agents at runtime. ripgrep + jq let agents
# query the pre-built /work/context JSON (the plan-mode prompt points them at
# grep/jq); bubblewrap is codex's sandbox (without it codex warns + falls back to
# a bundled copy every run).
RUN apt-get update && apt-get install -y --no-install-recommends \
      git \
      ripgrep \
      jq \
      bubblewrap \
      build-essential \
      python3 \
      python3-pip \
      python3-venv \
      curl \
      ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g yarn pnpm \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain — baseline language for Tasks self-verification. The agent
# runs `go build`/`go vet`/`go test` to check its edits before commit, and
# the `agentbox vendor` subcommand uses `go mod download` to pre-fetch
# module deps. dpkg arch keeps the multi-arch release builds (amd64/arm64)
# correct; GOTOOLCHAIN=local pins this version (no per-repo auto-download).
ARG GO_VERSION=1.24.11
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH
ENV GOTOOLCHAIN=local

# Non-root user with pre-configured per-user install prefixes, so
# runtime `npm install -g` and `pip install --user` work without root.
# /cache is pre-created + chowned so a fresh named volume the consumer
# mounts there (the vendor/agent shared module cache) inherits uid-1000
# ownership — otherwise the non-root container couldn't write to it.
RUN useradd -m -u 1000 agent \
    && mkdir -p /work /scratch /cache /home/agent/.npm-global \
    && chown -R agent:agent /work /scratch /cache /home/agent/.npm-global

ENV NPM_CONFIG_PREFIX=/home/agent/.npm-global
ENV NPM_CONFIG_UPDATE_NOTIFIER=false
ENV PATH=/home/agent/.npm-global/bin:/home/agent/.local/bin:$PATH

# Some agents shell out via a LOGIN shell (Codex uses `/bin/sh -lc`), which
# resets PATH from /etc/profile and drops the ENV PATH additions above — so
# `go`, the npm-global bin, and pip --user bins vanish for those commands
# (Codex's `go test` self-verify failed with "go: not found"). Re-export them
# via profile.d so login shells resolve the toolchain too. Written as root at
# build time; the runtime user `agent` can't write /etc/profile.d.
RUN printf 'export PATH=/usr/local/go/bin:/home/agent/.npm-global/bin:/home/agent/.local/bin:$PATH\n' \
      > /etc/profile.d/agentbox-path.sh

# Disable Claude Code non-essential outbound traffic. The master
# CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC variable kills telemetry
# (Datadog metrics — the traffic that was generating proxy-deny
# noise), Sentry error reporting, the /feedback command, and session
# quality surveys in one knob. DO_NOT_TRACK is the industry-standard
# equivalent — set both for belt-and-suspenders. DISABLE_AUTOUPDATER
# stays since auto-update checks aren't covered by the master switch.
# Per https://code.claude.com/docs/en/data-usage
ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
ENV DO_NOT_TRACK=1
ENV DISABLE_AUTOUPDATER=1

# Agent version pins. Overridable at build time via --build-arg or at
# runtime via docker run -e. The Go binary reads these on startup and
# installs the selected agent.
ARG CLAUDE_CODE_VERSION=2.1.141
ENV CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}
# Pinned @openai/codex version installed by the Codex Driver.Ensure on first
# container run. Bump deliberately (or override with --build-arg
# CODEX_VERSION=X.Y.Z) as Codex releases ship.
ARG CODEX_VERSION=0.136.0
ENV CODEX_VERSION=${CODEX_VERSION}
# Pinned opencode-ai (npm) version installed by the opencode Driver.Ensure on
# first container run. opencode is provider-agnostic; the model's provider
# prefix selects which API host the allowlist opens. Bump deliberately (or
# override with --build-arg OPENCODE_VERSION=X.Y.Z).
ARG OPENCODE_VERSION=1.17.9
ENV OPENCODE_VERSION=${OPENCODE_VERSION}

LABEL org.opencontainers.image.title="agentbox"
LABEL org.opencontainers.image.description="Open-source agent orchestrator"
LABEL org.opencontainers.image.source="https://github.com/deployment-io/agentbox"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.anthropic.claude-code.version="${CLAUDE_CODE_VERSION}"
LABEL com.openai.codex.version="${CODEX_VERSION}"
LABEL ai.opencode.version="${OPENCODE_VERSION}"

COPY --from=builder /out/agentbox /usr/local/bin/agentbox

USER agent
WORKDIR /work

ENTRYPOINT ["/usr/local/bin/agentbox"]
