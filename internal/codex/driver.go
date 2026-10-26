// Package codex provides the Driver implementation for OpenAI's Codex
// CLI. Registers itself with the agent package at init time; consumers
// side-effect-import this package to make "codex" resolvable via
// agent.DriverFor.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/qa"
	"github.com/deployment-io/agentbox/internal/review"
)

const agentType = "codex"

// finalMessageInstruction is appended to the user's prompt (Codex's
// `exec` has no system-prompt flag, so it rides on the prompt itself). It
// mirrors the Claude driver's contract exactly so the runner's downstream
// handling is agent-agnostic: a self-verify pass, then a final message
// ending with a changes summary, a <verify>{json}</verify> block, and a
// <pr_title>...</pr_title> trailer. The parser strips the two trailers and
// surfaces each as its own field.
//
// The <verify> paragraph is IDENTICAL in all three drivers, including the
// stderr_tail ask and the single-repository failure example — see the claude
// driver for what each part is for. It used to carry only the multi-repo
// example, so a one-repository failure on codex arrived as the command that
// failed and nothing about why. agent.TestVerifyParagraphIsIdenticalAcrossAgents
// compares the extracted paragraphs, so editing one driver's copy alone fails.
const finalMessageInstruction = `Before finishing: when the repo has a feasible build/test command (e.g. go build ./... && go vet ./..., go test ./..., tsc, pytest), run it to verify your edits and fix failures within your turn budget.

Final-message format. Your final message must contain, at the very end:

1. A multi-line changes summary describing what you changed and why, noting the verify outcome. This becomes the PR body's lead-in.

2. The verification result as compact JSON wrapped in <verify>...</verify>. If you ran build/test: {"ran":true,"passed":true|false,"command":"<command>"}. If you did not (no buildable code, docs-only, etc.): {"ran":false,"skipped_reason":"<why>"}. When passed is false, ALSO include "stderr_tail": the last few lines of actual failure output, verbatim — the error text, not your description of it. Example:

   <verify>{"ran":true,"passed":true,"command":"go build ./... && go vet ./..."}</verify>
   <verify>{"ran":true,"passed":false,"command":"go test ./...","stderr_tail":"pkg/auth/token.go:42:9: undefined: ParseJWT"}</verify>

   With more than one repository, add "steps" — one per repo, "repo" being its directory relative to the work dir — and keep the top-level fields as the rollup (passed = every step passed):

   <verify>{"ran":true,"passed":false,"command":"go test ./...","steps":[{"repo":"0-acme/api","command":"go test ./...","passed":false,"stderr_tail":"user_test.go:31: want 200, got 500"},{"repo":"1-acme/web","command":"npm test","passed":true}]}</verify>

   A failed verify blocks the commit and push, so fix what you can first.

3. A short PR title (≤72 chars, imperative mood, one line) wrapped in <pr_title>...</pr_title>. Example:

   <pr_title>Add OAuth login to auth-service</pr_title>

Emit <verify> and <pr_title> only here, at the very end.`

func init() {
	agent.Register(agentType, NewDriver)
}

// NewDriver constructs a Driver for the Codex CLI at the given pinned version.
func NewDriver(version string) agent.Driver {
	return &Driver{version: version}
}

// Driver installs and runs the Codex CLI via npm.
type Driver struct {
	version string
}

// AllowedHosts is the network allowlist Codex legitimately needs:
//   - api.openai.com — the OpenAI API endpoint codex calls
//   - registry.npmjs.org — the npm registry, hit by Driver.Ensure on first
//     container startup (`npm install -g @openai/codex`)
//
// If a deployment finds Codex needs more (e.g. an auth or telemetry
// endpoint), the org-level ADDITIONAL_ALLOWED_HOSTS env var unions on top.
func (d *Driver) AllowedHosts() []string {
	return []string{
		"api.openai.com",
		"registry.npmjs.org",
	}
}

// Ensure installs the Codex CLI globally via npm when it's not already on
// PATH. Mirrors the claude driver's install idiom; the Dockerfile ships
// Node 22 because @openai/codex's npm package requires Node >= 22.
func (d *Driver) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(d.Binary()); err != nil {
		pkg := "@openai/codex"
		if d.version != "" {
			pkg += "@" + d.version
		}
		fmt.Fprintf(os.Stderr, "[agentbox] using %s\n", pkg)
		cmd := exec.CommandContext(ctx, "npm", "install", "-g", "--silent", pkg)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("npm install -g %s failed: %w", pkg, err)
		}
	}
	return d.loginWithAPIKey(ctx)
}

// loginWithAPIKey registers OPENAI_API_KEY with the codex CLI via
// `codex login --with-api-key` (key on stdin), writing auth.json under
// CODEX_HOME (default ~/.codex). The CLI's auth state — and notably the
// app-server's responses websocket — authenticates from auth.json, NOT from
// the OPENAI_API_KEY env var: verified empirically on 0.136.0, where a valid
// key in the env alone still 401s on wss://api.openai.com/v1/responses while
// this login path authenticates cleanly. Idempotent (overwrites any prior
// login). Runs on every Ensure, including when the CLI is already installed.
// No key set → no-op (config validation already gates codex on the key; the
// claude-only harness path also lands here).
func (d *Driver) loginWithAPIKey(ctx context.Context) error {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, d.Binary(), "login", "--with-api-key")
	cmd.Stdin = strings.NewReader(key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex login --with-api-key failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) Binary() string {
	return "codex"
}

// BuildArgs assembles the headless `codex exec` invocation:
//
//   - --json: newline-delimited JSON events on stdout (consumed by the
//     OutputParser)
//   - --sandbox danger-full-access + --dangerously-bypass-approvals-and-sandbox:
//     fully autonomous — no approval prompts, full filesystem access —
//     mirroring Claude Code's --dangerously-skip-permissions. The container
//   - network proxy are the real sandbox. (--ask-for-approval is a
//     top-level flag, NOT a `codex exec` option, so it can't be used here.)
//   - --skip-git-repo-check: the runner checks the repo out into a SUBDIR of
//     WORK_DIR, so WORK_DIR itself isn't a git repo; without this `codex
//     exec` refuses to start.
//
// The prompt is passed as the trailing positional arg (per the documented
// `codex exec [FLAGS] "<prompt>"` form). Codex runs in cmd.Dir (WORK_DIR,
// set by the orchestrator), so no --cd is needed. Codex has no turn-cap or
// token-budget flag — those limits are enforced agentbox-side from the
// JSON event stream (see agent.Run's limit watcher).
// A REVIEW run swaps danger-full-access for --sandbox read-only and drops
// --dangerously-bypass-approvals-and-sandbox, so the sandbox is enforced rather
// than bypassed — but ONLY when the runner has not already made the
// repositories read-only. Codex implements read-only and workspace-write with
// bubblewrap, which cannot create namespaces inside agentbox's container (no
// capabilities, read-only rootfs), so every command the reviewer runs fails
// and the review examines nothing. When REVIEW_READONLY_MOUNTS says the
// runner mounted every repository read-only, that guarantee is already in the
// kernel and Codex's own sandbox is pure cost, so the review runs with the
// same flags an implement run uses. Without the variable — an older runner —
// read-only stays: a review that cannot run is safer than one that could
// write.
//
// A PREVIEW QA run (QA_TARGET preview, or unset) uses the same flags an
// implement run uses. Under --sandbox read-only bubblewrap fails in the
// container, so every shell command fails — and the preview QA prompt tells
// the agent to read the deploy facts in /work/context, which Codex can only do
// with its shell. Dropping the sandbox is safe: a preview QA run's /work is the
// runner's QA directory, which holds no repository — only agentbox's own files
// and the deployment context the runner binds READ-ONLY at /work/context — and
// the runner builds previews from its own copy of the Task, which is not
// mounted. Network still goes only through the egress proxy and its allowlist,
// and agentbox writes result.json after the agent exits, as in an implement
// run. The prompt still says not to edit, build or fix anything.
//
// A PRODUCTION QA run keeps --sandbox read-only: it checks live production
// sites and is report-only, and it has no use for a shell (no deployment
// context is mounted; its prompt mentions none), so it gets no shell that
// could reach a production host the egress allowlist admits (an org's
// ADDITIONAL_ALLOWED_HOSTS can) or run anything else. Any QA_TARGET other than
// production — including "" in a Config built by hand — is preview.
//
// Both QA targets register the MCP tools with approval mode "approve" — see
// mcpServerArgs.
func (d *Driver) BuildArgs(cfg *config.Config) []string {
	reviewing := cfg.Mode == config.ModeReview
	qaing := cfg.Mode == config.ModeQA
	productionQA := qaing && cfg.QATarget == config.QATargetProduction

	args := []string{
		"exec",
		"--json",
	}
	if (reviewing && !cfg.ReviewReadOnlyMounts) || productionQA {
		args = append(args, "--sandbox", "read-only")
	} else {
		args = append(args,
			"--sandbox", "danger-full-access",
			"--dangerously-bypass-approvals-and-sandbox",
		)
	}
	args = append(args,
		"--skip-git-repo-check",
		// Silence the non-essential outbound calls the agentbox proxy blocks
		// anyway, so they don't add deny-log noise or latency.
		//
		// features.plugins=false is the one that stops the startup denials
		// actually seen on the pinned 0.147.0 (github.com, api.github.com and
		// chatgpt.com, before the session starts). They are ALL the curated
		// plugin sync, which `codex exec` runs while creating the thread —
		// codex_core_plugins::{manager,startup_sync}, verified by running the
		// pinned binary behind a logging deny-proxy:
		//
		//   chatgpt.com/backend-api/plugins/featured?platform=codex
		//     — "warm featured plugin ids cache"
		//   git ls-remote https://github.com/openai/plugins.git
		//     — the curated sync itself
		//   api.github.com/repos/openai/plugins
		//     — its fallback when the git sync is refused
		//   chatgpt.com/backend-api/plugins/export/curated
		//     — its fallback when that is refused too
		//
		// One feature, four requests, one key. It does not touch MCP: the
		// mcp_servers.* registration below is core config, not a plugin
		// (verified with `codex mcp list` under the same override).
		//
		// The four keys below are NOT renamed and NOT dead — `codex exec
		// --strict-config` on 0.147.0 still accepts every one, and dropping
		// otel/analytics brings back a 60-second POST to
		// ab.chatgpt.com/otlp/v1/metrics. They simply never governed the
		// plugin sync. check_for_update_on_startup covers the TUI's update
		// check against api.github.com/repos/openai/codex/releases/latest,
		// which `codex exec` never performs; it stays because a future exec
		// path that does would be covered without another round of this.
		"-c", "features.plugins=false",
		"-c", "check_for_update_on_startup=false",
		"-c", "analytics.enabled=false",
		"-c", "otel.exporter=none",
		"-c", "otel.metrics_exporter=none",
	)
	if reviewing && cfg.ReviewEffort != "" {
		// Codex sends any string through unvalidated and the API rejects one
		// the model does not support, so only config's validated levels get
		// here.
		args = append(args, "-c", "model_reasoning_effort="+jsonValue(cfg.ReviewEffort))
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MCPSocket != "" && !reviewing {
		// Register the runner's tool socket as a stdio MCP server — the same
		// bridge the claude driver uses (`agentbox mcp-bridge <socket>`). Codex
		// has no --mcp-config flag; MCP servers live under mcp_servers.<name> in
		// its config, which we set with the same -c override mechanism used
		// above (values parse as JSON). --dangerously-bypass-approvals-and-sandbox
		// (set above) auto-allows the tools, so no approval prompt; a
		// production QA run, which has no bypass, needs an approval mode
		// instead, which every QA run sets (mcpServerArgs).
		// The pinned 0.158.0 registers servers from these -c overrides.
		args = append(args, mcpServerArgs(cfg)...)
	}
	// A review's and a QA run's prompt travels on stdin (see Stdin). With no
	// PROMPT argument, codex exec reads its instructions from stdin.
	if !reviewing && !qaing {
		args = append(args, cfg.StepPrompt+"\n\n"+trailingInstruction(cfg))
	}
	return args
}

// Stdin carries the review prompt plus the trailer instruction — codex has
// no system-prompt flag, so the instruction rides with the prompt exactly as
// it does in the argument-borne implement path. See the claude driver's
// Stdin for why a review prompt does not go in the args.
func (d *Driver) Stdin(cfg *config.Config) string {
	if cfg.Mode == config.ModeReview || cfg.Mode == config.ModeQA {
		return cfg.StepPrompt + "\n\n" + trailingInstruction(cfg)
	}
	return ""
}

// trailingInstruction picks which contract this run is held to — see the
// claude driver's copy. In review mode the implementer's instruction is not
// appended, so no <verify> or <pr_title> trailer is requested or produced.
func trailingInstruction(cfg *config.Config) string {
	switch cfg.Mode {
	case config.ModeReview:
		return review.Instruction(cfg)
	case config.ModeQA:
		return qa.Instruction(cfg)
	}
	return finalMessageInstruction
}

// mcpServerArgs registers the runner's tool socket as the stdio MCP server
// mcp_servers.deployment_io.
//
// A QA run, of either target, also sets the server's
// default_tools_approval_mode to "approve". A PRODUCTION QA run needs it: it
// runs under --sandbox read-only without
// --dangerously-bypass-approvals-and-sandbox, where `codex exec` behaves as
// approval policy "never" — and on the pinned 0.158.0 an MCP tool without
// read-only annotations then "requires approval, but approval policy is
// never" and is rejected before it reaches the server (verified end-to-end
// against the pinned binary with a fake model endpoint and MCP server). With
// "approve" the tools run without a prompt and the sandbox stays read-only.
// A PREVIEW QA run carries the bypass flag (see BuildArgs), so the key is
// harmless there. The implement path needs no such key: the bypass flag
// auto-allows the tools.
func mcpServerArgs(cfg *config.Config) []string {
	command, bridgeArgs := agent.BridgeCommand(cfg.MCPSocket)
	args := []string{
		"-c", "mcp_servers.deployment_io.command=" + jsonValue(command),
		"-c", "mcp_servers.deployment_io.args=" + jsonList(bridgeArgs),
	}
	if cfg.Mode == config.ModeQA {
		args = append(args, "-c", "mcp_servers.deployment_io.default_tools_approval_mode="+jsonValue("approve"))
	}
	return args
}

// jsonValue renders v as a JSON literal for a `codex -c key=<value>` override
// (codex parses the value as JSON, falling back to a bare string).
func jsonValue(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonList renders vs as a JSON array literal for a `codex -c key=<value>`
// override.
func jsonList(vs []string) string {
	b, _ := json.Marshal(vs)
	return string(b)
}

func (d *Driver) DetectVersion() string {
	out, err := exec.Command(d.Binary(), "--version").Output()
	if err != nil {
		return ""
	}
	// `codex --version` prints a line like "codex-cli 0.50.0"; keep the
	// first semver-looking token (starts with a digit).
	for _, f := range strings.Fields(string(out)) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' {
			return f
		}
	}
	return strings.TrimSpace(string(out))
}

func (d *Driver) NewOutputParser() agent.OutputParser {
	return newJSONLParser()
}

// NewLogFormatter turns Codex's `exec --json` JSONL into compact one-line
// summaries (session/turn markers, agent messages, commands, file edits) for
// the container log, while teeing the unfiltered stream to /scratch/agent.log
// for deep debugging. Non-JSON lines (npm output, proxy denies) pass through
// verbatim. Mirrors the claude driver's formatter.
func (d *Driver) NewLogFormatter(sink io.Writer) io.WriteCloser {
	return newHumanLogFormatter(sink, openRawStreamLog())
}

// Capabilities reports MCP tool support: Codex has no --mcp-config flag,
// so BuildArgs writes mcp_servers.deployment_io into its config via -c
// when cfg.MCPSocket is set.
func (d *Driver) Capabilities() agent.Capabilities {
	// ReviewCommands: on verified read-only mounts Codex runs without its own
	// sandbox (BuildArgs), so its shell works.
	return agent.Capabilities{MCPTools: true, ReviewCommands: true}
}
