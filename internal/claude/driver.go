// Package claude provides the Driver implementation for Anthropic's
// Claude Code CLI. Registers itself with the agent package at init time;
// consumers side-effect-import this package to make "claude-code"
// resolvable via agent.DriverFor.
package claude

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

// rawStreamLogPath is the in-container path that captures Claude Code's
// unfiltered stream-json for deep debugging. Created and chowned to the
// agent user by the Dockerfile; opening it outside the container (e.g.,
// during tests or local invocation) is expected to fail silently, in
// which case raw teeing is skipped.
const rawStreamLogPath = "/scratch/agent.log"

const agentType = "claude-code"

// finalMessageInstruction is appended to Claude Code's default system
// prompt. It first asks the agent to self-verify (run build/test when
// feasible — Tasks self-verification, see PLAN_tasks_verification.md),
// then specifies that the final assistant message carries three structured
// outputs at the end: a changes summary (the PR body lead-in), a
// <verify>{json}</verify> block with the agent's self-reported build/test
// outcome, and a short PR title wrapped in <pr_title>...</pr_title>. The
// parser strips the <verify> and <pr_title> blocks out of the result event
// and surfaces each as a separate field.
//
// The 72-char target matches Conventional Commits and GitHub's PR
// title soft limit. result.Write hard-caps to that on emit (with an
// ellipsis suffix) so consumers never see a runaway-length title even
// when the agent ignores the instruction.
//
// Kept deliberately terse: this prefix runs on every Claude Code call,
// so trimming directly reduces the per-Step token bill. One worked
// example is enough; the structure carries the rest — the failing-verify
// case is the one exception, and it earns its second example.
//
// stderr_tail is asked for ONLY when passed is false, so the common path
// costs nothing extra. Without it a failed Step reported just the command
// it ran — "agent self-verification failed: go build ./... && go vet
// ./... && go test ./..." — which names the ritual and not the cause, and
// left whoever was debugging to reproduce the failure themselves to find
// out what it even was. It cost a Task 23 minutes of finished work on
// 2026-08-29, where the answer turned out to be one pre-existing vet
// warning in a package the agent never opened.
//
// Verbatim output is the point: an agent's paraphrase of an error is the
// one thing the reader can already guess. The exact text is what makes it
// greppable and searchable.
const finalMessageInstruction = `Before finishing: when the repo has a feasible build/test command (e.g. go build ./... && go vet ./..., go test ./..., tsc, pytest), run it to verify your edits and fix failures within your turn budget.

Final-message format. Your final assistant message must contain, at the very end:

1. A multi-line changes summary describing what you changed and why, noting the verify outcome. This becomes the PR body's lead-in.

2. The verification result as compact JSON wrapped in <verify>...</verify>. If you ran build/test: {"ran":true,"passed":true|false,"command":"<command>"}. If you did not (no buildable code, docs-only, etc.): {"ran":false,"skipped_reason":"<why>"}. When passed is false, ALSO include "stderr_tail": the last few lines of actual failure output, verbatim — the error text, not your description of it. Example:

   <verify>{"ran":true,"passed":true,"command":"go build ./... && go vet ./..."}</verify>
   <verify>{"ran":true,"passed":false,"command":"go test ./...","stderr_tail":"pkg/auth/token.go:42:9: undefined: ParseJWT"}</verify>

3. A short PR title (≤72 chars, imperative mood, one line) wrapped in <pr_title>...</pr_title>. Example:

   <pr_title>Add OAuth login to auth-service</pr_title>

Emit <verify> and <pr_title> only here, at the very end.`

func init() {
	agent.Register(agentType, NewDriver)
}

// NewDriver constructs a Driver for Claude Code at the given pinned version.
func NewDriver(version string) agent.Driver {
	return &Driver{version: version}
}

// Driver installs and runs Claude Code via npm.
type Driver struct {
	version string
}

// AllowedHosts is the network allowlist Claude Code legitimately needs:
//   - api.anthropic.com — the Anthropic API endpoint Claude Code calls
//   - registry.npmjs.org — the npm registry, hit by Driver.Ensure on
//     first container startup (`npm install -g @anthropic-ai/claude-code`)
//
// Phase 8 (Bedrock support) will conditionally add Bedrock runtime
// endpoints (bedrock-runtime.<region>.amazonaws.com) when the env-var
// dispatch picks Bedrock instead of Anthropic Direct.
func (d *Driver) AllowedHosts() []string {
	return []string{
		"api.anthropic.com",
		"registry.npmjs.org",
	}
}

func (d *Driver) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(d.Binary()); err == nil {
		return nil
	}
	pkg := "@anthropic-ai/claude-code"
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
	return "claude"
}

func (d *Driver) BuildArgs(cfg *config.Config) []string {
	// Claude Code rejects --output-format=stream-json + -p without --verbose.
	args := []string{
		"-p", cfg.StepPrompt,
		"--append-system-prompt", finalMessageInstruction,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if cfg.MaxTurns != "" {
		args = append(args, "--max-turns", cfg.MaxTurns)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MCPSocket != "" {
		// Point Claude Code's MCP client at the runner's tool socket via the
		// `agentbox mcp-bridge` stdio bridge. --dangerously-skip-permissions
		// (set above) also auto-allows these tools, so the agent invokes them
		// without a permission prompt.
		args = append(args, "--mcp-config", mcpConfigJSON(cfg.MCPSocket))
	}
	return args
}

// mcpConfigJSON builds the inline --mcp-config value that registers the runner's
// tool channel as a stdio MCP server: this same binary run as
// `agentbox mcp-bridge <socket>`, which pipes JSON-RPC to the runner. Claude
// Code accepts a JSON string here (not only a file path). os.Executable
// resolves the bridge binary, falling back to the image's fixed path.
func mcpConfigJSON(socket string) string {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/usr/local/bin/agentbox"
	}
	return fmt.Sprintf(
		`{"mcpServers":{"deployment-io":{"command":%q,"args":["mcp-bridge",%q]}}}`,
		self, socket,
	)
}

func (d *Driver) DetectVersion() string {
	out, err := exec.Command(d.Binary(), "--version").Output()
	if err != nil {
		return ""
	}
	// `claude --version` prints "X.Y.Z (Claude Code)"; keep just the semver.
	if fields := strings.Fields(string(out)); len(fields) > 0 {
		return fields[0]
	}
	return strings.TrimSpace(string(out))
}

func (d *Driver) NewOutputParser() agent.OutputParser {
	return newStreamParser()
}

// NewLogFormatter returns a writer that translates Claude Code's
// stream-json into one-line summaries (model/init, thinking, tool
// calls, tool results, final outcome) before forwarding to sink, which
// is typically os.Stdout. If /scratch/agent.log is writable, the raw
// stream-json is also teed there so engineers can inspect the
// unfiltered output when an agent run misbehaves.
func (d *Driver) NewLogFormatter(sink io.Writer) io.WriteCloser {
	return newHumanLogFormatter(sink, openRawStreamLog())
}

func openRawStreamLog() io.WriteCloser {
	f, err := os.OpenFile(rawStreamLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	return f
}
