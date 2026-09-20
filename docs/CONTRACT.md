# agentbox Contract

agentbox runs an AI coding agent inside a Docker container against a
bind-mounted working directory and writes a structured result on exit.
Consumers spawn the container, inject env vars, read stdout/stderr as
logs, and read `/tmp/result.json` (or `$RESULT_PATH`) after exit.

## Inputs — Environment Variables

### Always required

| Variable | Description |
|---|---|
| `STEP_PROMPT` | The prompt the agent executes (batch mode). Free-form text. Required unless `AGENT_MODE=interactive`, where user turns arrive over the message pipe instead, or `AGENT_MODE=review`, where the work item is the diff agentbox computes itself. |
| `WORK_DIR` | Path to the bind-mounted working directory. Conventionally `/work`. agentbox validates that the directory exists before spawning the agent. |

### Credentials

| Variable | Description |
|---|---|
| `ANTHROPIC_API_KEY` | `sk-ant-...` string against `api.anthropic.com`. Required when `AGENT_TYPE=claude-code`. |
| `OPENAI_API_KEY` | OpenAI API key for Codex. Required when `AGENT_TYPE=codex`. The Codex CLI does NOT read this env var for request auth (verified on 0.136.0 — a valid key in the env alone still 401s); agentbox registers it at startup via `codex login --with-api-key`, which writes `auth.json`, the credential the CLI (including `app-server`) actually authenticates with. |
| `CODEX_API_KEY` | Legacy alias for `OPENAI_API_KEY` — nothing reads it directly; agentbox maps it onto `OPENAI_API_KEY` at startup when only it is set. Prefer `OPENAI_API_KEY`. |
| _(opencode, per-provider)_ | opencode is multi-provider — it authenticates with the API key for the provider named in the `MODEL` prefix. When `AGENT_TYPE=opencode`, provide the key matching the selected model's provider: `anthropic/…` → `ANTHROPIC_API_KEY`, `openai/…` → `OPENAI_API_KEY`, `openrouter/…` → `OPENROUTER_API_KEY`, `google/…` → `GEMINI_API_KEY`, plus `groq`/`xai`/`deepseek`/`mistral`. Unknown providers are lenient (opencode autodetects any provider key in the env and 401s at runtime if none matches). |

### Optional

| Variable | Description |
|---|---|
| `PREVIOUS_STEPS_SUMMARY` | Human-readable context of prior steps in a multi-step consumer scenario. agentbox passes it verbatim into the agent's prompt. |
| `MAX_TURNS` | Hard cap on agent turns. For `claude-code`, passed to `--max-turns`; for `codex` (no native flag) agentbox enforces it from the JSON event stream. Default: uncapped (trust wall-clock / no-activity detector). |
| `TOKEN_BUDGET` | Hard cap on cumulative input+output tokens, enforced agentbox-side from the event stream for agents without a native budget flag (e.g. `codex`). Default: `0` (uncapped). |
| `MODEL` | Override the agent's model. For `claude-code` e.g. `claude-sonnet-4-6`; for `codex` e.g. `gpt-5.5`; for `opencode` a provider-prefixed id e.g. `anthropic/claude-sonnet-4-6` or `openai/gpt-5`. Default: the agent's internal default. |
| `AGENT_TYPE` | Which agent to install and run: `claude-code` (default), `codex`, or `opencode`. Unsupported values are rejected at startup. |
| `CLAUDE_CODE_VERSION` | Pinned Claude Code version installed on first container run. Baked into the image as an ENV default; overridable at runtime for debugging. Ignored when `AGENT_TYPE` is not `claude-code`. |
| `CODEX_VERSION` | Pinned `@openai/codex` version installed on first container run (empty = latest). Ignored when `AGENT_TYPE` is not `codex`. |
| `OPENCODE_VERSION` | Pinned `opencode-ai` (npm) version installed on first container run (empty = latest). Baked into the image as an ENV default; overridable at runtime. Ignored when `AGENT_TYPE` is not `opencode`. |
| `NO_ACTIVITY_TIMEOUT` | Go duration string (e.g. `10m`, `90s`). If no agent output arrives within this window, agentbox kills the subprocess and exits with status `timeout` (exit code 4). Default: `10m`. Set to `0` to disable. |
| `RESULT_PATH` | Override where `/result.json` is written. Default: `/tmp/result.json`. |
| `ADDITIONAL_ALLOWED_HOSTS` | Comma-separated list of additional hostnames the agent can reach (e.g. `nexus.corp.local,api.linear.app`). Unioned with the active Driver's built-in allowlist (`api.anthropic.com,registry.npmjs.org` for `claude-code`). Empty / unset = only Driver-declared hosts are reachable. See [Network Restrictions](#network-restrictions). |
| `AGENTBOX_BLOCK_PRIVATE_IPS` | When `1` / `true` / unset (default): the proxy resolves each CONNECT target and rejects the request if any resolved IP is in a private/special range (RFC 1918, 169.254/16 cloud metadata, ULA, loopback, multicast, CGN, …). Closes the SSRF / metadata-IP-via-DNS attack class. Set to `0` / `false` / `no` for runners that legitimately need to reach internal-IP destinations (self-hosted GitLab on `10.0.x.x`, internal Nexus, etc.). See [Network Restrictions](#network-restrictions). |

### Interactive mode

A long-lived, bidirectional session (repo-aware chat) instead of one-shot
batch, selected with `AGENT_MODE=interactive`. `STEP_PROMPT` is not required
in this mode. claude-code and codex are supported (via `AGENT_TYPE`), each over
its own wire protocol: **claude-code** uses stream-json on stdin/stdout;
**codex** uses the App Server JSON-RPC (`codex app-server`). The driver hides
the difference — the filesystem I/O below is identical for both. **opencode**
is batch-only in v1 (no interactive wire yet); use `AGENT_MODE=batch`.

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
| `.agentbox-input/messages/<name>.json` | consumer → agentbox | one user turn `{"id","content","ts"}`; consumed (deleted) in filename order. Optional `"images"`: `[{"path","mediaType","width","height"}]`, images to attach to that turn — `path` is in-container (e.g. `/work/uploads/a1b2c3-1-shot.png`) and the consumer must write the file **before** the record. agentbox hands each one to the agent with the message (a base64 content block for **claude-code**, a `localImage` input item for **codex**); an image it cannot read is reported in the turn's text instead of dropping the turn. The key is additive — an older agentbox ignores it, and the turn still names the file in its text. |
| `.agentbox-output/messages/<seq>.json` | agentbox → consumer | assistant output `{"seq","type":"chunk"\|"final"\|"turn_end","text"}`; zero-padded `seq` so lexical order is chronological. A `turn_end` record (no `text`) follows the turn's last `final` — emitted when the agent finishes (or fails) a turn and is back to waiting for input, so the consumer can gate its composer on the boundary. |
| `.agentbox-output/task-spec.json` | agentbox → consumer | latest extracted task-spec (overwritten): the structured fields plus `raw`. |
| `.agentbox-output/repo-suggestion.json` | agentbox → consumer | latest repository suggestion the agent emitted (overwritten): `{"repositories":[{"name","reason","confidence"}]}`, at most 5 entries, `confidence` one of `high`\|`medium`\|`low`. Display data only — `name` is a lookup key the consumer resolves against its own repository list. Latest wins and the file is never cleared, so a turn that suggests nothing leaves the previous suggestion in place. Written only once the agent emits a block, so a consumer polling for it sees nothing until then. |
| `.agentbox-output/heartbeat.json` | agentbox → consumer | liveness `{"ts","turns","input_tokens","output_tokens"}` (overwritten ~every 30s). |

The session ends when the agent exits (e.g. `MAX_BUDGET_USD` reached), the
container receives SIGTERM (graceful: stdin is closed, then SIGTERM with a
10s grace), or `NO_ACTIVITY_TIMEOUT` elapses with no agent output.

### Review mode

A one-shot review of the change an implement run produced, selected with
`AGENT_MODE=review`. `STEP_PROMPT` is not required and is ignored: agentbox
computes the diff of each repository's working tree against the commit named
in `REVIEW_BASE_COMMITS`, decides which focused passes are worth running, and
builds the prompt itself. All three agents (`AGENT_TYPE`) can serve it.

In review mode the implementer's final-message instruction is NOT appended, so
no `<verify>` and no `<pr_title>` trailer is requested or parsed. The agent is
asked for a `<review>` block instead — see
[`review_result`](#review_result).

A review run reads NOTHING a previous run wrote: not `result.json`, not
`progress.json`, not the interactive message records, not a transcript. Its
prompt is the diff, the spec and the pass list. Consumers are expected to
enforce the same boundary structurally (the deployment.io runner moves the
implementer's `.agentbox-output` out of the work dir before each round), so
the guarantee does not rest on agentbox's restraint alone.

| Variable | Description |
|---|---|
| `AGENT_MODE` | `batch` (default), `interactive` or `review`. Any other value is rejected at startup. |
| `REVIEW_SPEC` | What the change is meant to achieve — JSON of the task spec, or the prose description when there is no structured spec. Passed into the prompt verbatim; agentbox does not parse it. Optional: without it the change is judged on its own terms. |
| `REVIEW_PASSES` | Comma-separated focused passes to run, e.g. `security,correctness`. Order is honoured. Empty / unset means `security,correctness`, the set this release ships. |
| `REVIEW_BASE_COMMITS` | **Required in review mode.** JSON object mapping each repository directory relative to `WORK_DIR` to the commit it was checked out at when the Step began, e.g. `{"0-acme/api":"9fceb02…"}`. THE BASELINE IS NOT HEAD: an agent may commit its own work, and diffing against HEAD on that path shows nothing at all. |
| `REVIEW_ROUND` | 1-based round number within one Step's review. Optional; absent or unreadable means `1`. |

**Pass selection is cost-gated by what the diff touches.** A diff whose every
changed path is documentation (`*.md`, `*.mdx`, `*.rst`, `*.txt`, `LICENSE`,
`docs/**`, image files) skips both passes; a diff whose every changed path is
a dependency lockfile (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`,
`go.sum`, `Cargo.lock`, `poetry.lock`, `Gemfile.lock`, `composer.lock`) runs
`security` and skips `correctness`; an empty diff skips both. A skipped pass
is recorded in `coverage` with its reason — never silently omitted. When every
pass is skipped, no agent is spawned at all and the run succeeds with the
coverage record alone.

**The diff is capped** at 400000 bytes overall and 60000 bytes per file, with
explicit elision markers where content was dropped. Truncation is recorded in
the `coverage` reason of every pass that ran, because a pass that saw part of
a change reached a partial verdict and must not be reported as complete.

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
  "denied_hosts": ["pypi.org", "files.pythonhosted.org"],
  "pr_title": "Add OAuth login to auth-service",
  "verify_result": {
    "ran": true,
    "passed": false,
    "command": "go test ./...",
    "duration_ms": 41230,
    "stdout_tail": "…",
    "stderr_tail": "user_test.go:31: want 200, got 500",
    "skipped_reason": "docs-only change",
    "pre_existing": true,
    "steps": [
      {
        "repo": "0-acme/api",
        "command": "go test ./...",
        "passed": false,
        "stdout_tail": "…",
        "stderr_tail": "user_test.go:31: want 200, got 500",
        "baseline_ran": true,
        "baseline_passed": false,
        "baseline_stderr_tail": "user_test.go:31: want 200, got 500"
      },
      {
        "repo": "1-acme/web",
        "command": "npm test",
        "passed": true
      }
    ]
  }
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

#### `verify_result`

The agent's own build/test check, run before it declares itself done, plus
agentbox's comparison of any failure against the state the run started from.
Omitted entirely when the agent reported nothing.

| Field | Written by | Meaning |
|---|---|---|
| `ran` | agent | Whether a verification was run at all. `false` means none was attempted — a docs-only change, no detectable build command — and `skipped_reason` says why. Consumers do **not** gate on a skipped verify. |
| `passed` | agent | Rollup verdict: true only when every step passed. |
| `command` | agent | The representative command line. |
| `duration_ms` | agent | Wall-clock of the agent's own verification. |
| `stdout_tail` / `stderr_tail` | agent | Capped tails of the failure output, verbatim. Asked for only when `passed` is false. |
| `skipped_reason` | agent | One-liner, present only when `ran` is false. |
| `steps` | agent | Per-repository breakdown; see below. Omitted by the agent for a single-repository run, in which case agentbox synthesises the one step from the rollup before replaying it. A failed step always forces the rollup `passed` to false. |
| `pre_existing` | **agentbox** | Whether every failed step also failed on the baseline; see below. |

`command`, `passed` and the tails remain the ROLLUP whether or not `steps` is
present, so a consumer written before `steps` existed behaves exactly as it
did before.

Each entry in `steps` describes one repository's verification:

| Field | Written by | Meaning |
|---|---|---|
| `repo` | agent | The repository directory **relative to `WORK_DIR`** as the agent sees it, e.g. `0-acme/api`. |
| `command` | agent | The command line run for that repository. |
| `passed` | agent | That repository's verdict. |
| `stdout_tail` / `stderr_tail` | agent | Capped tails for that repository. |
| `baseline_ran` | **agentbox** | Whether a baseline comparison was actually carried out. |
| `baseline_passed` | **agentbox** | Whether the same command passed on the baseline. Meaningful only when `baseline_ran` is true. |
| `baseline_stderr_tail` | **agentbox** | Output of the baseline run, present when the baseline also failed. |

**Baseline** means *the commit each repository was checked out at when
agentbox started* — recorded before the agent subprocess is spawned, not read
back afterwards. It is not the repository's base branch, and it is not HEAD at
the end of the run: an agent is allowed to commit its own work, so HEAD at the
end can be entirely the agent's change, and replaying there would reproduce
the agent's own failure and misreport it as pre-existing.

For every FAILED step, agentbox materialises that commit with `git worktree
add --detach` under `<WORK_DIR>/.agentbox-tmp`, re-runs the step's command
there with the agent's environment and caches (plus `GOWORK=off`, and the
agent's `node_modules` / `.venv` symlinked in rather than reinstalled), and
records the verdict on the step. The agent's own working tree is never
touched — no stash, no checkout, and the worktree is removed and pruned
afterwards. Each replay is bounded by a per-step timeout (10m) and the run as
a whole by an overall replay budget (30m).

The fields marked **agentbox** above are written by agentbox alone. The
`<verify>` block is agent-authored JSON, so agentbox discards any value the
agent supplied for them before replaying — an agent cannot assert its own
failure is pre-existing.

`pre_existing` is true only when EVERY failed step reported `baseline_ran:
true` and `baseline_passed: false` — i.e. the run inherited all of its
failures rather than introducing any. It is failure-closed: a step with no
recorded start commit, an unresolvable `repo` path, a commit that is no longer
reachable, or a replay that errored or timed out reports `baseline_ran: false`
and prevents `pre_existing` from being set. Passing steps are never replayed,
and the whole pass only runs when `status` is `success`.

Consumers are expected to gate a commit / push on `ran && !passed &&
!pre_existing`, and to surface a pre-existing failure to the user instead of
discarding the run's work.

#### `review_result`

What a review-mode run found and what it actually looked at. Present only for
`AGENT_MODE=review`; omitted entirely otherwise.

```json
"review_result": {
  "findings": [
    {
      "key": "sec-auth-missing",
      "parameter": "security",
      "severity": "high",
      "location": "0-acme/api/handler.go:41",
      "what": "the new /export handler does not check the caller's session",
      "why": "any unauthenticated caller can read another org's data",
      "stage": "review",
      "pass": "security"
    }
  ],
  "coverage": [
    {"parameter": "security", "state": "checked"},
    {"parameter": "correctness", "state": "checked"},
    {"parameter": "spec conformance", "state": "not checked", "reason": "no pass for this parameter in this release"}
  ]
}
```

| Field | Written by | Meaning |
|---|---|---|
| `findings[].key` | agent | Short stable slug for the finding, so the same finding is recognisable across rounds after a fix. |
| `findings[].parameter` | agent | One of `security`, `correctness`, `spec conformance`, `testing`, `deploy readiness`, `performance`, `maintainability`, `reliability`. A NAME, not a number: agentbox imports no consumer's enum, so the consumer parses the name (case, spaces, hyphens and underscores ignored) and drops what it cannot read. |
| `findings[].severity` | agent | One of `info`, `low`, `medium`, `high`, `critical`. |
| `findings[].location` | agent | Where in the change, e.g. `0-acme/api/handler.go:41`. |
| `findings[].what` / `why` | agent | What was seen, and why it matters. Both are needed: what alone leaves the reader to work out whether it matters, why alone leaves them hunting for where. |
| `findings[].stage` | **agentbox** | Always `review` for a review run, stamped regardless of what the agent emitted. An agent cannot relabel where its finding came from. |
| `findings[].pass` | agent | Which focused pass produced it. |
| `coverage[].parameter` | **agentbox** | Every one of the eight parameters appears exactly once. |
| `coverage[].state` | **agentbox** | `checked` (a pass ran), `skipped` (a pass stood down — see `reason`) or `not checked` (this release ships no pass for it). Built from what actually ran, not from the agent's claim; the agent's own claim is honoured only when it ADMITS a gap agentbox could not see. |
| `coverage[].reason` | **agentbox** | Why a pass was skipped, or that the diff was truncated. |

**`must_fix_open` is not part of this contract and never will be.** Whether
findings block the change is a policy decision the consumer owns, made from
its own severity thresholds; agentbox has no struct field for it, so an agent
cannot assert that its own findings need not be fixed.

**Caps applied at extraction**, mirroring the consumer's storage limits: at
most 100 findings, one coverage entry per parameter, 120-rune `key`, 400-rune
`location`, 1000-rune `what`, 1000-rune `why`, 60-rune `pass`, 400-rune
`reason`. Every cap TRUNCATES in runes rather than rejecting — a review that
found 400 things is still worth its first 100.

The `<review>` block is extracted by the same machine-owned-block rules as the
interactive task-spec: the tag must be on a line of its own, openings pair
with the nearest following close scanning newest-first (so a prose mention
cannot swallow a real block), the latest valid block wins, and every block is
stripped from `changes_summary` so it can never leak into a pull-request body.

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
