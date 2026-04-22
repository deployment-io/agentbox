# agentbox

[![Build & Test](https://github.com/deployment-io/agentbox/actions/workflows/build.yml/badge.svg)](https://github.com/deployment-io/agentbox/actions/workflows/build.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Run AI coding agents in a Docker container with a predictable contract.**

agentbox is an open-source orchestrator that runs an AI coding agent
(Claude Code in v1) against a bind-mounted working directory. You give
it a prompt and credentials via environment variables; it installs the
agent, runs it, streams its output to stdout, and writes a structured
result to `/result.json`. Consumers don't have to think about stream-json
parsing, subprocess lifecycle, signal handling, or version pinning —
agentbox handles it so the same invocation shape works across CI, a
managed platform, or a local terminal.

It's the execution runtime behind [deployment.io](https://deployment.io)
Tasks but is designed to stand alone — useful to anyone running agents
headlessly on their own infrastructure.

## Quick Start

You'll need:
- Docker
- A working directory (any git repo or folder for the agent to work on)
- An Anthropic API key (or AWS Bedrock credentials — see [Credentials](#credentials))

```bash
# Pull the image
docker pull deploymentio/agentbox:latest

# Create a scratch output dir (agentbox writes result.json there)
mkdir -p /tmp/agentbox-out
chmod 777 /tmp/agentbox-out

# Run the agent on a local directory
docker run --rm \
  -e ANTHROPIC_API_KEY="sk-ant-..." \
  -e STEP_PROMPT="Add a README.md if missing, summarizing the project." \
  -e RESULT_PATH="/scratch/result.json" \
  -v "$(pwd):/work" \
  -v /tmp/agentbox-out:/scratch \
  deploymentio/agentbox:latest

# Inspect the outcome
cat /tmp/agentbox-out/result.json
```

On exit:
- Files the agent created or modified are in your working directory
- `/tmp/agentbox-out/result.json` has the structured outcome (`status`,
  `changes_summary`, `files_changed`, `token_usage`, `turns`, etc.)
- The container's exit code indicates what happened (see [Contract](#contract))

## Contract

Full spec: [docs/CONTRACT.md](docs/CONTRACT.md). Summary:

### Required environment variables

| Variable | Description |
|---|---|
| `STEP_PROMPT` | The prompt for the agent. Free-form text. |
| One of the [credential](#credentials) paths | Anthropic Direct or AWS Bedrock. |

### Optional environment variables

| Variable | Default | Description |
|---|---|---|
| `WORK_DIR` | `/work` | Path where the repo is bind-mounted. |
| `RESULT_PATH` | `/tmp/result.json` | Where to write the structured result. |
| `AGENT_TYPE` | `claude-code` | Which agent to install and run. |
| `CLAUDE_CODE_VERSION` | Pinned in image | Overridable Claude Code version. |
| `MODEL` | Agent default | e.g., `opus`, `haiku`, or a pinned version. |
| `MAX_TURNS` | Uncapped | Hard cap on agent turns. |
| `NO_ACTIVITY_TIMEOUT` | `20m` | Kill the subprocess if stdout is silent this long. `0` disables. |
| `PREVIOUS_STEPS_SUMMARY` | — | Free-form context of prior steps for multi-step workflows. |

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Execution failure |
| `2` | Auth / rate-limit / model-access failure |
| `3` | Cancelled (SIGTERM received) |
| `4` | No-activity timeout |

### `/result.json` schema (abbreviated)

```json
{
  "schema_version": 1,
  "agent_type": "claude-code",
  "agent_version": "2.1.117",
  "status": "success",
  "changes_summary": "Added README.md summarizing the project.",
  "files_changed": ["/work/README.md"],
  "token_usage": {
    "input_tokens": 4,
    "output_tokens": 125,
    "cache_read_tokens": 28143
  },
  "turns": 2
}
```

## Credentials

agentbox accepts exactly one of two credential paths:

### Anthropic Direct

```bash
-e ANTHROPIC_API_KEY="sk-ant-..."
```

Get a key at [console.anthropic.com](https://console.anthropic.com).

### AWS Bedrock

```bash
-e CLAUDE_CODE_USE_BEDROCK=1 \
-e AWS_ACCESS_KEY_ID="..." \
-e AWS_SECRET_ACCESS_KEY="..." \
-e AWS_SESSION_TOKEN="..." \
-e AWS_REGION="us-west-2"
```

Requires Anthropic Claude model access enabled in your AWS Bedrock
console for the selected region. Temporary credentials (from EC2
instance metadata or `aws sts assume-role`) work; `AWS_SESSION_TOKEN`
is optional for long-lived credentials.

## Supported Agents

**v1:** Claude Code only.

**Planned:** other agent runtimes (Codex, Aider, …) will register through
the same `Installer` interface and dispatch on `AGENT_TYPE`. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## How It Works

```
Docker container
  └── agentbox (Go binary — ENTRYPOINT, PID 1)
       └── agent subprocess (claude in v1)
            └── agent's own subprocesses (bash, git, npm, ...)
```

- Pre-built language runtimes (Node.js 20, Python 3) live in the image
  at build time.
- At startup, the Go orchestrator installs the selected agent package
  (`npm install -g` / `pip install --user`) as a non-root user. This
  takes ~15-30s on a cold cache.
- The agent runs against the bind-mounted working directory. Its
  stdout is teed to the container's stdout (for log streaming) and to
  an internal parser that builds the structured result.
- On SIGTERM, agentbox forwards it to the agent with a 10s grace
  period before SIGKILL.
- On exit, `/result.json` is written and the container exits with the
  appropriate code.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for details.

## Building From Source

```bash
git clone https://github.com/deployment-io/agentbox.git
cd agentbox
docker build -t agentbox:dev .
```

The multi-stage Dockerfile compiles the Go binary inside a
`golang:1.24-bookworm` stage and copies it into a
`debian:bookworm-slim` runtime. No local Go install required.

To pin a different Claude Code version at build time:

```bash
docker build --build-arg CLAUDE_CODE_VERSION=X.Y.Z -t agentbox:dev .
```

## Platform Support

v1 publishes `linux/amd64` images only. Multi-arch support is
planned but not yet in scope.

## Status

**Pre-release.** Functionally complete for v1.0.0; image and contract
are stable. First public release pending.

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Related

- [docs/CONTRACT.md](docs/CONTRACT.md) — full input/output contract
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — internals
- [deployment.io](https://deployment.io) — the platform agentbox was
  built to power (and one of many possible consumers)
