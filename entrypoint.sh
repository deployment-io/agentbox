#!/bin/sh
set -e

# Install Claude Code if not present, then hand off to the orchestrator.
# Runtime install (not baked in) for licensing — see docs/ARCHITECTURE.md.
# A shared npm cache across container spawns makes this a no-op after first run.

if ! command -v claude >/dev/null 2>&1; then
  echo "[agentbox] Installing Claude Code ${CLAUDE_CODE_VERSION}..." >&2
  npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
fi

exec /usr/local/bin/agentbox "$@"
