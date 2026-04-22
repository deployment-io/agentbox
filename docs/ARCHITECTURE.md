# agentbox Architecture

## Overview

agentbox is a Docker container that runs an AI coding agent (Claude
Code in v1) against a bind-mounted working directory. It receives
credentials and a prompt via environment variables, spawns the agent,
streams its output, and writes a structured result file on exit.

## Container Model

```
Container (one Docker container, one PID namespace, one filesystem)
  ├── entrypoint.sh (install agent if needed, exec orchestrator)
  └── agentbox (Go binary — effective PID 1)
       └── claude (subprocess via os/exec)
            └── agent subprocesses (bash, git, npm, python, etc.)
```

Single container, subprocess pattern — no Docker-in-Docker.

## Claude Code Distribution Model

agentbox does not bundle Claude Code in the published image. At
container startup, `entrypoint.sh` runs `npm install -g
@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}` inside the user's
container, pulling from Anthropic's official npm registry.

Cost: ~15-30 seconds of startup latency per container. Mitigable via a
shared npm cache volume across spawns.

### Version Pinning

Each published agentbox image tag corresponds to one pinned Claude Code
version. Inspect it without running the container via the image's
`com.anthropic.claude-code.version` label.

For one-off builds against a different version:

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
