#!/bin/sh
set -e

# agentbox entrypoint. Runs as user 1000 (agent).
#
# Responsibility: ensure Claude Code is available on PATH, then exec the
# agentbox orchestrator binary. The orchestrator handles env validation,
# subprocess lifecycle, signal forwarding, and result.json writing.
#
# Claude Code is installed at runtime here (not baked into the image) for
# licensing reasons — see docs/ARCHITECTURE.md → "Claude Code Distribution
# Model". On subsequent container spawns against the same npm cache
# (e.g., via a shared cache volume), this install is effectively a no-op.

if ! command -v claude >/dev/null 2>&1; then
  echo "[agentbox] Installing Claude Code ${CLAUDE_CODE_VERSION}..." >&2
  npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
fi

exec /usr/local/bin/agentbox "$@"
