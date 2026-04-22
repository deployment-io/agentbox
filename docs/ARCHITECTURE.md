# agentbox Architecture

**Status:** v0 draft. Stable for v1.0.0 release.

## Overview

agentbox is a Docker container that runs an AI coding agent (Claude
Code in v1) as a subprocess against a bind-mounted working directory.
It receives credentials and a prompt via environment variables, spawns
the agent, streams its output, and writes a structured result file on
exit.

agentbox is the execution runtime used by [deployment.io](https://deployment.io)
Tasks, but it's designed to stand alone and be useful to anyone running
agents headlessly on their own infrastructure.

## Container Model

```
Container (one Docker container, one PID namespace, one filesystem)
  ├── entrypoint.sh (brief: install agent if needed, exec orchestrator)
  └── agentbox (Go binary — effective PID 1)
       └── claude (Node subprocess via os/exec)
            └── agent subprocesses (bash, git, npm, python, etc.)
```

No nested containers. No Docker-in-Docker. Subprocess pattern throughout.

### Why single container

- Container isolation is between (orchestrator + agent) and the host.
  No value in isolating the orchestrator from the agent — they
  cooperate.
- Docker-in-Docker needs privileged containers + socket mounts. Breaks
  security posture.
- Coordinating two containers is operational mess. Lifecycle management,
  log forwarding, bind-mount synchronization, signal propagation — none
  of which we need.
- Signal handling is direct: `cmd.Process.Signal(syscall.SIGTERM)`
  reaches Claude Code immediately.
- Log streaming is one hop: agent stdout → orchestrator stdout → Docker
  captures → consumer reads via `ContainerLogs`.

## Claude Code Distribution Model

v1 does NOT bundle Claude Code in the published image. At container
startup, `entrypoint.sh` runs
`npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}`
inside the user's container, pulling from Anthropic's official npm
registry.

### Why runtime install

- `@anthropic-ai/claude-code` is published under Anthropic's Commercial
  Terms as proprietary software. Redistributing it in a public Docker
  image without explicit approval is legally risky.
- Runtime install has the same legal posture as any Docker image
  running `apt-get install` or `pip install` at startup: the user's
  environment installs the software; agentbox just automates the call.
- End-users are already bringing their own Anthropic credentials;
  install and use both happen in their environment.

### Cost

~15-30 seconds of additional startup latency per container (npm install
over the network). Mitigable via a per-runner shared npm cache volume
if consumers opt in. Not typically a concern for Tasks that run
minutes-to-hours.

### Pinning discipline

`CLAUDE_CODE_VERSION` is set as an ENV in the Dockerfile. Each agentbox
release corresponds to one Claude Code version. Users who need a
specific Claude Code version pin agentbox's image tag accordingly.

### Future: bundled image (pending Anthropic approval)

Bundling Claude Code at image build time would give faster startup,
stronger reproducibility, and offline capability. Outreach is underway
to Anthropic asking for explicit approval to bundle under our scenario
(users bring their own credentials, no resale). If approved, the
Dockerfile gains a `RUN npm install -g @anthropic-ai/claude-code@...`
line and entrypoint.sh's install step is removed. Consumer-visible
contract is unchanged.

## Trust Boundary

- agentbox does not persist any environment variable it receives.
- Credentials live only in the process environment for the lifetime of
  the container.
- `/result.json` contains no secrets.
- The image contains no pre-baked tokens, keys, or user data.
- Container runs as a non-root user (UID 1000) inside the container.

Consumers are responsible for the host-side sandbox (Docker run flags,
network policy, bind-mount restrictions, CPU/memory limits). See
consumer documentation for specifics.

## Signal Handling

On SIGTERM or SIGINT, agentbox cancels the context passed to
`exec.CommandContext`, which sends SIGKILL to the Claude Code subprocess
(Go's default). For graceful shutdown with a grace period, forward the
signal first and SIGKILL only after a timeout — planned for v1.0.0
polish (not yet in v0).

On any termination path, agentbox attempts to write a structured
`/result.json` so consumers can distinguish "cancelled" from "crashed".

SIGKILL against agentbox itself can't be handled — `/result.json` will
be missing. Consumers treat a missing result file as a distinct failure
mode (e.g., runner crash, OOM kill).

## Result Schema Versioning

`/result.json` always includes `"schema_version": N`. Consumers check
this field before parsing further. Bumps are versioned:

- **v1:** current shape (see docs/CONTRACT.md). Will be frozen at v1.0.0
  agentbox release.
- Future versions: added only when consumer-visible changes are
  unavoidable. Fields are added liberally without bumping schema_version;
  renamed/removed fields require a bump.
