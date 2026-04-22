# agentbox Contract

**Status:** v0 draft. Stable for v1.0.0 release.

agentbox runs an AI coding agent inside a Docker container against a
bind-mounted working directory and writes a structured result on exit.
Consumers spawn the container, inject env vars, read stdout/stderr as
logs, and read `/tmp/result.json` (or `$RESULT_PATH`) after exit.

## Inputs — Environment Variables

### Always required

| Variable | Description |
|---|---|
| `STEP_PROMPT` | The prompt the agent executes. Free-form text. |
| `WORK_DIR` | Path to the bind-mounted working directory. Conventionally `/work`. agentbox validates that the directory exists before spawning the agent. |

### Credentials — exactly one of two paths

agentbox validates that exactly one credential path is provided and
fails fast otherwise.

**Anthropic Direct:**

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | `sk-ant-...` string against `api.anthropic.com`. |

**AWS Bedrock:**

| Variable | Description |
|---|---|
| `CLAUDE_CODE_USE_BEDROCK` | Must be `1` to opt into the Bedrock path. |
| `AWS_ACCESS_KEY_ID` | Required. |
| `AWS_SECRET_ACCESS_KEY` | Required. |
| `AWS_SESSION_TOKEN` | Required for temporary credentials (typical from EC2 instance metadata). |
| `AWS_REGION` | Required. |

**Google Vertex AI** is not supported in v1. (`CLAUDE_CODE_USE_VERTEX`
+ GCP service account credentials will be added in v2+.)

### Optional

| Variable | Description |
|---|---|
| `PREVIOUS_STEPS_SUMMARY` | Human-readable context of prior steps in a multi-step consumer scenario. agentbox passes it verbatim into the agent's prompt. |
| `MAX_TURNS` | Hard cap on agent turns. Default: uncapped (trust wall-clock / no-activity detector). |
| `MODEL` | Override default model (e.g. `opus`, `haiku`, or a pinned version). Default: Claude Code's internal default. |
| `AGENT_TYPE` | v1 accepts only `claude-code` (default). v2+ will dispatch to Codex, Aider, etc. |
| `RESULT_PATH` | Override where `/result.json` is written. Default: `/tmp/result.json`. |

### Deliberately NOT in the contract

- Task / Step / run identifiers. agentbox doesn't use them functionally.
  Consumers correlate runs externally based on which container they
  spawned — no identifier threading through the env.

## Working Directory

Bind-mounted read-write at `$WORK_DIR` (default `/work`). The agent
reads and modifies files here. agentbox does not chown, scrub, or
pre-process it.

agentbox **never writes outside `$WORK_DIR`** except for the result
file (default `/tmp/result.json`).

## Outputs

### stdout / stderr

Claude Code's output, forwarded verbatim. Consumers capture via Docker
`ContainerLogs`.

### `/tmp/result.json` (or `$RESULT_PATH`)

Written on exit. Schema:

```json
{
  "schema_version": 1,
  "agent_type": "claude-code",
  "agent_version": "<pinned version>",
  "status": "success" | "failure" | "cancelled" | "timeout",
  "changes_summary": "Short natural-language description of what was changed",
  "files_changed": ["path/to/file.ts"],
  "token_usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cache_read_tokens": 0
  },
  "turns": 0,
  "error": "error description"
}
```

The `error` field is omitted on success. All other fields are always
populated.

Consumers read the result file after the container exits. Recommended
approaches:
- Bind-mount a host path and set `RESULT_PATH` to it
- `docker cp <container>:/tmp/result.json ./` after exit (works even
  after the container has stopped, before it's removed)

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Execution failure (agent ran but reported an error) |
| `2` | Auth / rate-limit failure (distinct so consumers can surface "update your credentials" cleanly) |
| `3` | Cancelled (SIGTERM received, clean shutdown) |
| `4` | Timeout (no-activity detector fired) |

## Signal Handling

- **SIGTERM / SIGINT:** agentbox forwards the signal to the Claude Code
  subprocess, waits 10 seconds for clean exit, then SIGKILLs. Writes
  `/tmp/result.json` with `status: "cancelled"` before exiting.
- **SIGKILL (against agentbox itself):** can't be handled; `/result.json`
  will be missing. Consumers treat "missing result" as a distinct failure.
