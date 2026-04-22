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

### Version Pinning Policy

`CLAUDE_CODE_VERSION` is pinned to an exact version in the Dockerfile
as both a build-time `ARG` (overridable) and runtime `ENV` (visible to
entrypoint.sh and Claude Code's own tooling). Each published agentbox
image tag corresponds to exactly one Claude Code version. The version
is also recorded in the image's `com.anthropic.claude-code.version`
label so consumers can inspect without running the container.

**Why exact pins (not floating / semver ranges):**
- Reproducibility. A bug reported against `agentbox:v1.0.0` is about
  the exact Claude Code version baked into that release — not whatever
  happened to be latest on npm when you rebuilt.
- Deterministic debugging. Upgrade is an explicit action with a visible
  diff, not a silent change that broke someone's CI.
- Supply-chain discipline. Pinning narrows the window between "Claude
  Code published a bad version" and "agentbox users picked it up."

**When to bump:**
- Security advisories for Claude Code.
- Bug fixes we rely on for correctness.
- New features the agentbox contract or a consumer needs.
- Otherwise, avoid churn. Pinning is the point — don't bump just
  because a newer version exists.

**How to bump:**
1. Update `ARG CLAUDE_CODE_VERSION=X.Y.Z` in the Dockerfile.
2. Build + test the image against a representative scenario.
3. Release a new agentbox image tag (per SemVer rules below).
4. Update the CHANGELOG with the old → new Claude Code version.

**agentbox SemVer in relation to Claude Code version:**
- A Claude Code version bump alone is a patch release of agentbox
  (e.g., `v1.0.0` → `v1.0.1`) unless the new Claude Code version
  introduces behavior that breaks the agentbox contract.
- Contract-visible changes (new required env var, result.json schema
  bump, exit code change) are minor or major releases per SemVer.

**Build-time override:**

```sh
docker build \
  --build-arg CLAUDE_CODE_VERSION=2.1.114 \
  -t agentbox:custom .
```

Useful for testing a different version without editing the Dockerfile.
Not recommended for production — prefer pulling the matching published
tag.

**Runtime override (advanced, not recommended):**

Consumers *can* override `CLAUDE_CODE_VERSION` at `docker run` time,
which causes entrypoint.sh to install a different version at startup.
This produces an image whose label lies (says vX but runs vY) and
defeats the pinning rationale — use it only for one-off debugging of
version-regression scenarios, and don't publish images that depend on
runtime overrides.

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
