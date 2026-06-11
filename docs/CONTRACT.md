# agentbox Contract

agentbox runs an AI coding agent inside a Docker container against a
bind-mounted working directory and writes a structured result on exit.
Consumers spawn the container, inject env vars, read stdout/stderr as
logs, and read `/tmp/result.json` (or `$RESULT_PATH`) after exit.

## Inputs — Environment Variables

### Always required

| Variable | Description |
|---|---|
| `STEP_PROMPT` | The prompt the agent executes (batch mode). Free-form text. Required unless `AGENT_MODE=interactive`, where user turns arrive over the message pipe instead. |
| `WORK_DIR` | Path to the bind-mounted working directory. Conventionally `/work`. agentbox validates that the directory exists before spawning the agent. |

### Credentials

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | `sk-ant-...` string against `api.anthropic.com`. Required when `AGENT_TYPE=claude-code`. |
| `OPENAI_API_KEY` | OpenAI API key for Codex. Required when `AGENT_TYPE=codex`. The Codex CLI does NOT read this env var for request auth (verified on 0.136.0 — a valid key in the env alone still 401s); agentbox registers it at startup via `codex login --with-api-key`, which writes `auth.json`, the credential the CLI (including `app-server`) actually authenticates with. |
| `CODEX_API_KEY` | Legacy alias for `OPENAI_API_KEY` — nothing reads it directly; agentbox maps it onto `OPENAI_API_KEY` at startup when only it is set. Prefer `OPENAI_API_KEY`. |

### Optional

| Variable | Description |
|---|---|
| `PREVIOUS_STEPS_SUMMARY` | Human-readable context of prior steps in a multi-step consumer scenario. agentbox passes it verbatim into the agent's prompt. |
| `MAX_TURNS` | Hard cap on agent turns. For `claude-code`, passed to `--max-turns`; for `codex` (no native flag) agentbox enforces it from the JSON event stream. Default: uncapped (trust wall-clock / no-activity detector). |
| `TOKEN_BUDGET` | Hard cap on cumulative input+output tokens, enforced agentbox-side from the event stream for agents without a native budget flag (e.g. `codex`). Default: `0` (uncapped). |
| `MODEL` | Override the agent's model. For `claude-code` e.g. `claude-sonnet-4-6`; for `codex` e.g. `gpt-5.5`. Default: the agent's internal default. |
| `AGENT_TYPE` | Which agent to install and run: `claude-code` (default) or `codex`. Unsupported values are rejected at startup. |
| `CLAUDE_CODE_VERSION` | Pinned Claude Code version installed on first container run. Baked into the image as an ENV default; overridable at runtime for debugging. Ignored when `AGENT_TYPE` is not `claude-code`. |
| `CODEX_VERSION` | Pinned `@openai/codex` version installed on first container run (empty = latest). Ignored when `AGENT_TYPE` is not `codex`. |
| `NO_ACTIVITY_TIMEOUT` | Go duration string (e.g. `10m`, `90s`). If no agent output arrives within this window, agentbox kills the subprocess and exits with status `timeout` (exit code 4). Default: `10m`. Set to `0` to disable. |
| `RESULT_PATH` | Override where `/result.json` is written. Default: `/tmp/result.json`. |
| `ADDITIONAL_ALLOWED_HOSTS` | Comma-separated list of additional hostnames the agent can reach (e.g. `nexus.corp.local,api.linear.app`). Unioned with the active Driver's built-in allowlist (`api.anthropic.com,registry.npmjs.org` for `claude-code`). Empty / unset = only Driver-declared hosts are reachable. See [Network Restrictions](#network-restrictions). |
| `AGENTBOX_BLOCK_PRIVATE_IPS` | When `1` / `true` / unset (default): the proxy resolves each CONNECT target and rejects the request if any resolved IP is in a private/special range (RFC 1918, 169.254/16 cloud metadata, ULA, loopback, multicast, CGN, …). Closes the SSRF / metadata-IP-via-DNS attack class. Set to `0` / `false` / `no` for runners that legitimately need to reach internal-IP destinations (self-hosted GitLab on `10.0.x.x`, internal Nexus, etc.). See [Network Restrictions](#network-restrictions). |

### Interactive mode

A long-lived, bidirectional session (repo-aware chat) instead of one-shot
batch, selected with `AGENT_MODE=interactive`. `STEP_PROMPT` is not required
in this mode. Both agents are supported (via `AGENT_TYPE`), each over its own
wire protocol: **claude-code** uses stream-json on stdin/stdout; **codex**
uses the App Server JSON-RPC (`codex app-server`). The driver hides the
difference — the filesystem I/O below is identical for both.

| Variable | Description |
|---|---|
| `AGENT_MODE` | `batch` (default) or `interactive`. |
| `SESSION_ID` | Stable session id forwarded as `claude --session-id` (must be a valid UUID) so the transcript persists and can be resumed after a container restart. Optional but recommended. |
| `READ_ONLY` | `1` / `true` / `yes` restricts the agent to read-only investigation. **claude-code**: a tool allowlist (`Read`, `Grep`, `Glob`, safe read-only `Bash(...)` patterns) with `--dangerously-skip-permissions` omitted so the allowlist is enforced. **codex**: the app-server read-only sandbox with `approvalPolicy: never`. Default: off. |
| `MAX_BUDGET_USD` | Cap total spend for the session (`claude --max-budget-usd`); the agent self-exits when reached. Default: uncapped. |
| `APPEND_SYSTEM_PROMPT_FILE` | Path to a file whose contents are appended to the agent's system prompt. **claude-code**: passed inline via `--append-system-prompt` (the CLI has no `-file` variant). **codex**: prepended to the first user turn (the app-server has no separate system-prompt channel). |

Per-agent applicability: `READ_ONLY` and `APPEND_SYSTEM_PROMPT_FILE` apply to
both agents. `SESSION_ID` and `MAX_BUDGET_USD` are **claude-code** only in v1
— codex's app-server manages its own thread id, and codex budget capping is
not yet wired.

**Filesystem I/O** (under `$WORK_DIR`), all written atomically (temp + rename):

| Path | Direction | Contents |
|---|---|---|
| `.agentbox-input/messages/<name>.json` | consumer → agentbox | one user turn `{"id","content","ts"}`; consumed (deleted) in filename order. |
| `.agentbox-output/messages/<seq>.json` | agentbox → consumer | assistant output `{"seq","type":"chunk"\|"final"\|"turn_end","text"}`; zero-padded `seq` so lexical order is chronological. A `turn_end` record (no `text`) follows the turn's last `final` — emitted when the agent finishes (or fails) a turn and is back to waiting for input, so the consumer can gate its composer on the boundary. |
| `.agentbox-output/task-spec.json` | agentbox → consumer | latest extracted task-spec (overwritten): the structured fields plus `raw`. |
| `.agentbox-output/heartbeat.json` | agentbox → consumer | liveness `{"ts","turns","input_tokens","output_tokens"}` (overwritten ~every 30s). |

The session ends when the agent exits (e.g. `MAX_BUDGET_USD` reached), the
container receives SIGTERM (graceful: stdin is closed, then SIGTERM with a
10s grace), or `NO_ACTIVITY_TIMEOUT` elapses with no agent output.

### Not in the contract

- Task / Step / run identifiers.

## Working Directory

Bind-mounted read-write at `$WORK_DIR` (default `/work`). The agent
reads and modifies files here. agentbox does not chown, scrub, or
pre-process it, and writes nothing outside `$WORK_DIR` except the
result file.

## Outputs

### stdout / stderr

Compact, one-line-per-event human-readable summaries derived from the
agent's native output. For Claude Code this means stream-json events
are translated to lines like:

```
[init] model=claude-opus-4-7 tools=15 mcp_servers=0 cwd=/work
[thinking] Let me start by reading the file.
[tool] Bash: ls -la /work
[result] (success, 104 bytes) total 16 …
[tool] Edit: /work/main.go
[done] status=ok turns=4 tokens=1.2k/350 duration=23.1s summary=…
```

On a failed run the `[done]` line reads `status=error` and, when the
agent reported a specific failure subtype, carries a `reason=` token —
e.g. `[done] status=error reason=error_max_turns turns=26 …` — so the
cause is visible without opening `/result.json`.

Per-message usage counters, session IDs, UUIDs, parent-tool-use IDs,
and thinking signatures are dropped — they're encryption material or
debugging hooks, never useful to a human reader. Lines that don't
parse as the agent's native event format (npm install output, proxy
deny logs, Node stack traces) pass through verbatim.

Stderr is forwarded verbatim. Consumers capture both via Docker
`ContainerLogs`.

The unfiltered raw stream from the agent is also written to
`/scratch/agent.log` inside the container for deep debugging when the
summarized view isn't enough. Bind-mount `/scratch` to expose it to
the host.

### `<result-dir>/progress.json` (live snapshot)

Written periodically (~every 3s) into the same directory as
`/result.json` while the agent is running. Atomic: each update goes
through `progress.json.tmp` + rename, so consumers never observe a
partially-written file. Schema:

```json
{
  "schema_version": 1,
  "updated_at_unix": 1714859123,
  "turns": 12,
  "input_tokens": 30000,
  "output_tokens": 5000,
  "cache_read_tokens": 100000
}
```

The file is meant for *in-flight* polling — typically by an
orchestrator that wants to surface a live progress UI. Final values
are also present in `/result.json`'s `turns` and `token_usage` fields,
so consumers that don't need live counters can ignore `progress.json`
entirely. Removed at container exit (cleaned up alongside the rest of
the work directory by the orchestrator); not part of the persistent
output.

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
    "cache_read_tokens": 0,
    "cache_creation_tokens": 0
  },
  "turns": 0,
  "cost_usd": 0.0421,
  "error": "error description",
  "denied_hosts": ["pypi.org", "files.pythonhosted.org"]
}
```

`cost_usd` is the agent's self-reported total run cost in US dollars. It
is present only for agents that emit one — Claude Code reports it
(`total_cost_usd` in its stream-json result event); Codex reports token
usage only, so the field is omitted for `codex` runs and consumers
estimate cost from `token_usage` and the published per-model rates.

The `error` field is omitted on success; all other fields are always
populated. On failure it carries the most specific detail available, in
priority order: a tailored reason for known causes (e.g. a turn-limit
exhaustion reads `claude reached its turn limit after 26 turns; raise
max_turns to allow more steps`), else the agent's own error description,
else the failure-subtype name (e.g. `error_during_execution`), falling
back to the exit status plus a tail of stderr only when the agent
crashed before reporting anything. A bare `exit status N` is never the
whole story when the agent told us more. `denied_hosts` is omitted when no allowlist denies happened
during the run.

`denied_hosts` lists hostnames the in-process CONNECT proxy refused
because they weren't on the active allowlist (Driver-declared ∪
`ADDITIONAL_ALLOWED_HOSTS`). Surfaced so consumers can suggest
allowlist additions without parsing stderr — see [Network
Restrictions](#network-restrictions). Other proxy deny categories
(IP-literal, non-443 port, non-CONNECT method, private-IP block) are
intentionally NOT included; those represent agent bugs or
security-gate violations rather than allowlist gaps.

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

## Network Restrictions

agentbox starts an HTTP CONNECT proxy on `127.0.0.1:<random-port>`
before installing the agent and exports `HTTP_PROXY`, `HTTPS_PROXY`,
`NO_PROXY` env vars to its own process so all child processes (the
install command, the agent itself) inherit and route through it.

The proxy enforces a hostname allowlist on outbound HTTPS (port 443)
CONNECT requests:

- **Driver-declared hosts** — each agent ships with its required
  hostnames (Claude Code: `api.anthropic.com`, `registry.npmjs.org`).
- **`ADDITIONAL_ALLOWED_HOSTS`** — comma-separated user additions
  (org-level or per-deploy), unioned with Driver-declared.

Anything outside the union is rejected with HTTP 403 + a log line on
stderr. Plain HTTP (non-CONNECT) and non-port-443 CONNECTs are also
rejected — modern HTTPS adoption makes this a reasonable simplification.

Beyond the hostname allowlist, the proxy applies these checks:

- **IP-literal CONNECTs are rejected** (e.g., `CONNECT 169.254.169.254:443`).
  Forces every request through DNS, where the resolved address can be
  validated.
- **Resolved IPs are validated against a private-IP deny-list** (RFC 1918,
  169.254/16, ULA, loopback, multicast, CGN, class-E reserved). An
  allowlisted hostname that resolves to one of these is rejected with
  HTTP 403. Disable per-runner with `AGENTBOX_BLOCK_PRIVATE_IPS=0`.
- **Dial uses the validated IP literal**, not the hostname. Defeats DNS
  rebinding between the validation lookup and the upstream dial.
- **Hostnames are normalized** (case-folded, whitespace-trimmed, trailing
  dot stripped) before allowlist lookup so `api.anthropic.com.` doesn't
  bypass an `api.anthropic.com` entry.
- **Loopback and wildcard hostnames are hard-denied** (`localhost`,
  `127.0.0.1`, `0.0.0.0`, `::1`) regardless of allowlist contents —
  defends against fat-fingered allowlist entries.
- **Concurrency cap and CONNECT-handshake timeout** bound resource
  exposure; a slow/silent client is dropped and its slot reclaimed.

**Limits of the protection:** the proxy only catches HTTP/HTTPS traffic
that respects standard `HTTP_PROXY` env vars (most modern SDKs do —
Anthropic SDK, npm, pip, requests, fetch, curl). An agent that opens
raw sockets directly (rare in practice) would bypass. Defense in depth
at the Docker network layer (cloud-metadata block via `ExtraHosts`,
future iptables enforcement) covers the bypass case.
