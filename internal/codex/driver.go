// Package codex provides the Driver implementation for OpenAI's Codex
// CLI. Registers itself with the agent package at init time; consumers
// side-effect-import this package to make "codex" resolvable via
// agent.DriverFor.
package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

const agentType = "codex"

// finalMessageInstruction is appended to the user's prompt (Codex's
// `exec` has no system-prompt flag, so it rides on the prompt itself). It
// mirrors the Claude driver's contract exactly so the runner's downstream
// handling is agent-agnostic: a self-verify pass, then a final message
// ending with a changes summary, a <verify>{json}</verify> block, and a
// <pr_title>...</pr_title> trailer. The parser strips the two trailers and
// surfaces each as its own field.
const finalMessageInstruction = `Before finishing: when the repo has a feasible build/test command (e.g. go build ./... && go vet ./..., go test ./..., tsc, pytest), run it to verify your edits and fix failures within your turn budget.

Final-message format. Your final message must contain, at the very end:

1. A multi-line changes summary describing what you changed and why, noting the verify outcome. This becomes the PR body's lead-in.

2. The verification result as compact JSON wrapped in <verify>...</verify>. If you ran build/test: {"ran":true,"passed":true|false,"command":"<command>"}. If you did not (no buildable code, docs-only, etc.): {"ran":false,"skipped_reason":"<why>"}. Example:

   <verify>{"ran":true,"passed":true,"command":"go build ./... && go vet ./..."}</verify>

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
	if _, err := exec.LookPath(d.Binary()); err == nil {
		return nil
	}
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
func (d *Driver) BuildArgs(cfg *config.Config) []string {
	args := []string{
		"exec",
		"--json",
		"--sandbox", "danger-full-access",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		// Silence the non-essential outbound calls the agentbox proxy
		// blocks anyway, so they don't add deny-log noise or latency: the
		// GitHub update check (github.com) and the Statsig analytics /
		// feature-flag traffic (chatgpt.com / ab.chatgpt.com).
		"-c", "check_for_update_on_startup=false",
		"-c", "analytics.enabled=false",
		"-c", "otel.exporter=none",
		"-c", "otel.metrics_exporter=none",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, cfg.StepPrompt+"\n\n"+finalMessageInstruction)
	return args
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
