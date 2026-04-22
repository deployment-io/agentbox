# ---- Stage 1: Build agentbox binary ----
FROM golang:1.24-bookworm AS builder

WORKDIR /src
COPY . .

# Once the module acquires external dependencies, split this into
# `COPY go.mod go.sum ./ && RUN go mod download && COPY . .` so the
# dep-download layer can cache independently of source changes.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/agentbox ./cmd/agentbox


# ---- Stage 2: Runtime ----
FROM debian:bookworm-slim

# Language runtimes and build tools needed by supported agents.
# Node: Claude Code (npm-packaged).
# Python: Aider and future pip-packaged agents (v2+).
# build-essential, git, curl: used by agents at runtime.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git \
      build-essential \
      python3 \
      python3-pip \
      python3-venv \
      curl \
      ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Non-root user with pre-configured per-user install prefixes, so
# runtime `npm install -g` and `pip install --user` work without root.
RUN useradd -m -u 1000 agent \
    && mkdir -p /work /home/agent/.npm-global \
    && chown -R agent:agent /work /home/agent/.npm-global

ENV NPM_CONFIG_PREFIX=/home/agent/.npm-global
ENV NPM_CONFIG_UPDATE_NOTIFIER=false
ENV PATH=/home/agent/.npm-global/bin:/home/agent/.local/bin:$PATH

# Agent version pins. Overridable at build time via --build-arg or at
# runtime via docker run -e. The Go binary reads these on startup and
# installs the selected agent.
ARG CLAUDE_CODE_VERSION=2.1.117
ENV CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}

LABEL org.opencontainers.image.title="agentbox"
LABEL org.opencontainers.image.description="Open-source agent orchestrator"
LABEL org.opencontainers.image.source="https://github.com/deployment-io/agentbox"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.anthropic.claude-code.version="${CLAUDE_CODE_VERSION}"

COPY --from=builder /out/agentbox /usr/local/bin/agentbox

USER agent
WORKDIR /work

ENTRYPOINT ["/usr/local/bin/agentbox"]
