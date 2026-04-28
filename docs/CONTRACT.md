# agentbox Contract

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
| `AGENT_TYPE` | Which agent to install and run. v1 supports only `claude-code` (default). v2+ adds other agents. Unsupported values are rejected at startup. |
| `CLAUDE_CODE_VERSION` | Pinned Claude Code version installed on first container run. Baked into the image as an ENV default; overridable at runtime for debugging. Ignored when `AGENT_TYPE` is not `claude-code`. |
| `NO_ACTIVITY_TIMEOUT` | Go duration string (e.g. `10m`, `90s`). If no agent output arrives within this window, agentbox kills the subprocess and exits with status `timeout` (exit code 4). Default: `10m`. Set to `0` to disable. |
| `RESULT_PATH` | Override where `/result.json` is written. Default: `/tmp/result.json`. |

### Not in the contract

- Task / Step / run identifiers.

## Working Directory

Bind-mounted read-write at `$WORK_DIR` (default `/work`). The agent
reads and modifies files here. agentbox does not chown, scrub, or
pre-process it, and writes nothing outside `$WORK_DIR` except the
result file.

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

The `error` field is omitted on success; all other fields are always
populated.

To read the result file from the host, bind-mount a path and point
`RESULT_PATH` at it, or `docker cp` the default path after exit.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Execution failure (agent ran but reported an error) |
| `2` | Auth / rate-limit failure (distinct so consumers can surface "update your credentials" cleanly) |
| `3` | Cancelled (SIGTERM received, clean shutdown) |
| `4` | Timeout (no-activity detector fired) |

## Signal Handling

- **SIGTERM / SIGINT:** forwarded to the subprocess with a grace period
  before SIGKILL. Exits with `status: "cancelled"`, code 3.
- **SIGKILL against agentbox:** can't be handled; `/result.json` will be
  missing. Consumers treat that as a distinct failure.
