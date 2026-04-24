# agentbox Architecture

## Overview

agentbox is a Docker container that runs an AI coding agent (Claude
Code in v1) against a bind-mounted working directory. It receives
credentials and a prompt via environment variables, installs the
selected agent, spawns it, streams its output, and writes a structured
result file on exit.

## Container Model

```
Container (one Docker container, one PID namespace, one filesystem)
  └── agentbox (Go binary — ENTRYPOINT, PID 1)
       └── agent subprocess (claude in v1) via os/exec
            └── agent's own subprocesses (bash, git, npm, python, etc.)
```

Single container, subprocess pattern — no Docker-in-Docker, no
entrypoint shell script. The Go binary handles env validation,
agent installation, subprocess lifecycle, signal forwarding, and
result.json writing.

## Image

Built via multi-stage Dockerfile:

- **Stage 1 (`golang:1.24-bookworm`):** compiles the agentbox Go binary
  statically for `linux/amd64`.
- **Stage 2 (`debian:bookworm-slim`):** minimal runtime. Contains the
  language runtimes agents need (Node.js 20 for npm-based agents,
  Python 3 + pip for pip-based agents), `git`, and `build-essential`.
  The agentbox binary is copied from Stage 1. Runs as a non-root user
  (UID 1000).

The published runtime image is ~500 MB. The builder stage doesn't ship.

## Agent Install at Startup

Each supported agent has an `Installer` that knows:

- how to install the agent package (e.g., `npm install -g
  @anthropic-ai/claude-code@<version>` for Claude Code)
- what binary to exec (`claude`, `aider`, etc.)
- how to build its command-line arguments
- how to detect the installed version

At container startup, agentbox reads `AGENT_TYPE` from the env,
resolves the installer, and runs `installer.Ensure()` — a no-op if the
agent is already present, otherwise an install from the official
package registry (npm / pypi). Install output goes to stderr so it
appears in container logs.

Language runtimes are pre-installed at image build time (not at
startup), so `npm install` and `pip install --user` run as the
non-root user without privilege escalation. Cold-start latency is
dominated by the agent-package install (~15-30s for Claude Code on a
cold npm cache).

Agent packages are installed from Anthropic's / PyPI's official
registries at runtime by the user's container — agentbox does not
redistribute proprietary agent code in its image.

## Version Pinning

Each agent's version is pinned as a Docker `ARG` (build-time default,
overridable via `--build-arg`) and exposed as an `ENV` so the Go
binary sees it at runtime. The pinned version is also recorded in an
image label (e.g., `com.anthropic.claude-code.version`).

For a one-off build against a different version:

```sh
docker build --build-arg CLAUDE_CODE_VERSION=X.Y.Z -t agentbox:custom .
```

## Trust Boundary

- Credentials live only in the process environment for the lifetime of
  the container. No persistence.
- `/result.json` contains no secrets.
- The image carries no pre-baked tokens, keys, or user data.
- The orchestrator and agent run as a non-root user (UID 1000).

Host-side sandboxing (Docker run flags, network policy, bind mounts,
resource limits) is the consumer's responsibility.

## Signal Handling

See [CONTRACT.md](CONTRACT.md) for the observable behavior. SIGKILL
against agentbox itself can't be handled — `/result.json` will be
missing. Consumers treat a missing result file as a distinct failure
mode (e.g., OOM kill).
