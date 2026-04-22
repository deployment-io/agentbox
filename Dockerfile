FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git \
      build-essential \
      python3 \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Claude Code is installed at container startup by entrypoint.sh, not
# baked in. See docs/ARCHITECTURE.md → "Claude Code Distribution Model"
# and "Version Pinning Policy".
ARG CLAUDE_CODE_VERSION=2.1.117
ENV CLAUDE_CODE_VERSION=${CLAUDE_CODE_VERSION}

LABEL org.opencontainers.image.title="agentbox"
LABEL org.opencontainers.image.description="Open-source agent orchestrator for Claude Code"
LABEL org.opencontainers.image.source="https://github.com/deployment-io/agentbox"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.anthropic.claude-code.version="${CLAUDE_CODE_VERSION}"
LABEL com.anthropic.claude-code.install="runtime"

COPY bin/agentbox /usr/local/bin/agentbox
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/agentbox

RUN useradd -m -u 1000 agent
USER agent
WORKDIR /work

ENTRYPOINT ["/entrypoint.sh"]
