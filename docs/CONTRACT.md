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
| `ADDITIONAL_ALLOWED_HOSTS` | Comma-separated list of additional hostnames the agent can reach (e.g. `nexus.corp.local,api.linear.app`). Unioned with the active Driver's built-in allowlist (`api.anthropic.com,registry.npmjs.org` for `claude-code`). Empty / unset = only Driver-declared hosts are reachable. See [Network Restrictions](#network-restrictions). |
| `AGENTBOX_BLOCK_PRIVATE_IPS` | When `1` / `true` / unset (default): the proxy resolves each CONNECT target and rejects the request if any resolved IP is in a private/special range (RFC 1918, 169.254/16 cloud metadata, ULA, loopback, multicast, CGN, …). Closes the SSRF / metadata-IP-via-DNS attack class. Set to `0` / `false` / `no` for runners that legitimately need to reach internal-IP destinations (self-hosted GitLab on `10.0.x.x`, internal Nexus, etc.). See [Network Restrictions](#network-restrictions). |

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
    "cache_read_tokens": 0
  },
  "turns": 0,
  "error": "error description",
  "denied_hosts": ["pypi.org", "files.pythonhosted.org"]
}
```

The `error` field is omitted on success; all other fields are always
populated. `denied_hosts` is omitted when no allowlist denies happened
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
