// Package opencode provides the Driver implementation for opencode — the
// open-source, provider-agnostic AI coding agent (https://opencode.ai, MIT,
// maintained by Anomaly/SST). Registers itself with the agent package at init
// time; consumers side-effect-import this package to make "opencode" resolvable
// via agent.DriverFor.
//
// PROTOTYPE STATUS. The Driver itself (install / exec / version / allowlist) is
// high-confidence and mirrors the codex driver. The OutputParser's event field
// names (parser.go) are derived from opencode's documented `run --format json`
// stream and MUST be confirmed against a captured run — see the "VERIFY AGAINST
// REAL OUTPUT" note there and PLAN_tasks_opencode_support.md.
//
// What makes opencode worth a third driver: it is provider-agnostic. The model
// is a "provider/model" id (e.g. "anthropic/claude-sonnet-4-6"), so one agent
// can target Anthropic, OpenAI, OpenRouter, Google, local Ollama, etc. — the
// bring-your-own-model agent for the BYO-cloud story.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/review"
)

const agentType = "opencode"

// opencodeConfigEnv points opencode at a custom config file (it is loaded
// between the global and project configs). The driver writes a minimal config
// there granting full tool autonomy.
const opencodeConfigEnv = "OPENCODE_CONFIG"

// bridgeDiscoveryTimeoutMS is how long opencode may spend fetching the
// bridge's tool list before giving up and loading none. Generous over
// opencode's 5s default because the cost of expiring is a silently
// tool-less run rather than an error, and the cost of a larger value is
// only a slower failure in the case where the bridge is genuinely dead.
const bridgeDiscoveryTimeoutMS = 30000

// finalMessageInstruction is appended to the user's prompt so opencode's final
// message ends with a changes summary, a <verify>{json}</verify> block, and a
// <pr_title>...</pr_title> trailer. It is byte-for-byte the contract the claude
// and codex drivers use, which is what keeps the runner's downstream handling
// agent-agnostic. The parser strips the two trailers and surfaces each as its
// own field.
//
// NOTE (Rule of Three): opencode is the third agent to carry this verbatim.
// Extract it (plus the trailer parsers in parser.go) into internal/agent/ in a
// follow-up — see PLAN_tasks_opencode_support.md.
const finalMessageInstruction = `Before finishing: when the repo has a feasible build/test command (e.g. go build ./... && go vet ./..., go test ./..., tsc, pytest), run it to verify your edits and fix failures within your turn budget.

Final-message format. Your final message must contain, at the very end:

1. A multi-line changes summary describing what you changed and why, noting the verify outcome. This becomes the PR body's lead-in.

2. The verification result as compact JSON wrapped in <verify>...</verify>. If you ran build/test: {"ran":true,"passed":true|false,"command":"<command>"}. If you did not (no buildable code, docs-only, etc.): {"ran":false,"skipped_reason":"<why>"}. Example:

   <verify>{"ran":true,"passed":true,"command":"go build ./... && go vet ./..."}</verify>

   With more than one repository, add "steps" — one per repo, "repo" being its directory relative to the work dir — and keep the top-level fields as the rollup (passed = every step passed):

   <verify>{"ran":true,"passed":false,"command":"go test ./...","steps":[{"repo":"0-acme/api","command":"go test ./...","passed":false,"stderr_tail":"user_test.go:31: want 200, got 500"},{"repo":"1-acme/web","command":"npm test","passed":true}]}</verify>

   A failed verify blocks the commit and push, so fix what you can first.

3. A short PR title (≤72 chars, imperative mood, one line) wrapped in <pr_title>...</pr_title>. Example:

   <pr_title>Add OAuth login to auth-service</pr_title>

Emit <verify> and <pr_title> only here, at the very end.`

func init() {
	agent.Register(agentType, NewDriver)
}

// NewDriver constructs a Driver for opencode at the given pinned version.
func NewDriver(version string) agent.Driver {
	return &Driver{version: version}
}

// Driver installs and runs opencode via the opencode-ai npm package.
type Driver struct {
	version string
}

// AllowedHosts is the network allowlist opencode legitimately needs. Unlike the
// single-endpoint claude/codex drivers, opencode is multi-provider, so the API
// host depends on the selected model — it can't be a constant. The static infra
// hosts plus the provider host derived from the MODEL env var form the base;
// anything missing (exotic providers, telemetry) is unioned on by the org-level
// ADDITIONAL_ALLOWED_HOSTS, and any blocked host is surfaced to the user via
// result.json's denied_hosts.
//
// MODEL is read from the env directly because the Driver interface's
// AllowedHosts() takes no cfg; this is the same var config.Load reads. A cleaner
// long-term fix is to thread the model into the Driver at construction.
func (d *Driver) AllowedHosts() []string {
	hosts := []string{
		"registry.npmjs.org", // npm metadata + tarball, hit by Driver.Ensure
		// The opencode-ai postinstall may fetch a platform binary from GitHub
		// releases — best-effort; confirm on first run and trim if unused.
		"github.com",
		"objects.githubusercontent.com",
		// opencode's model + pricing registry, fetched at runtime. BOTH hosts,
		// because which one is used depends on the opencode version:
		//
		//	models.dev           older releases
		//	models.opencode.ai   1.18.x — opencode's own hosted copy
		//
		// A live 1.18.16 run failed with `denied:models.opencode.ai` while
		// models.dev sat in this list unused. opencode cannot resolve a model
		// id without the registry, so losing it fails EVERY opencode run
		// regardless of provider — and the version that changes it is pinned in
		// the Dockerfile, so a version bump alone can break this.
		//
		// Keeping both costs nothing: an unused allowlist entry opens no
		// connection. Do not "tidy" one away without checking the pinned
		// version — that is exactly how this broke.
		"models.dev",
		"models.opencode.ai",
	}
	if h := providerHostFromModel(os.Getenv("MODEL")); h != "" {
		hosts = append(hosts, h)
	}
	return hosts
}

// providerHostFromModel maps the "provider/" prefix of an opencode model id to
// the API host opencode will call. Providers not in this table (or a model
// without a provider prefix) contribute no host — the lenient path, covered by
// ADDITIONAL_ALLOWED_HOSTS. Keep roughly aligned with opencodeProviderEnvKey in
// internal/config (kept separate to avoid an import cycle: config is imported by
// drivers, not the reverse).
func providerHostFromModel(model string) string {
	provider, _, found := strings.Cut(model, "/")
	if !found {
		return ""
	}
	switch provider {
	case "anthropic":
		return "api.anthropic.com"
	case "openai":
		return "api.openai.com"
	case "openrouter":
		return "openrouter.ai"
	// "novita-ai", not "novita" — this switches on opencode's own provider id,
	// the key in models.opencode.ai, which is what appears before the slash in
	// MODEL. Our catalogue's provider key is "novita"; the two are different
	// strings for the same thing and only this one belongs here.
	case "novita-ai":
		return "api.novita.ai"
	case "google":
		return "generativelanguage.googleapis.com"
	case "groq":
		return "api.groq.com"
	case "xai":
		return "api.x.ai"
	case "deepseek":
		return "api.deepseek.com"
	case "mistral":
		return "api.mistral.ai"
	}
	return ""
}

// Ensure installs opencode globally via npm when it's not already on PATH, then
// writes the autonomy config. Mirrors the codex/claude install idiom; the
// Dockerfile ships Node 22, which the opencode-ai npm wrapper needs.
func (d *Driver) Ensure(ctx context.Context) error {
	if _, err := exec.LookPath(d.Binary()); err != nil {
		pkg := "opencode-ai"
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
	return writeAutonomousConfig()
}

// writeAutonomousConfig writes a minimal opencode config granting full tool
// autonomy and points OPENCODE_CONFIG at it. Headless runs must never block on a
// permission prompt — the container sandbox + network allowlist are the real
// guardrails, mirroring claude's --dangerously-skip-permissions and codex's
// --dangerously-bypass-approvals-and-sandbox. Set via env (not a CLI flag) so
// it also covers permission classes that default to "ask" (e.g.
// external_directory, which matters because the repo is checked out into a
// subdir of WORK_DIR). agent.Run snapshots os.Environ() after Ensure, so the
// exported var reaches the opencode subprocess.
// A REVIEW run gets the read-only config instead: edits and shell commands are
// denied outright, leaving opencode's own read / grep / glob tools, which is
// everything a reviewer needs. AGENT_MODE is read from the env for the same
// reason the socket is — Ensure takes no *config.Config, and this file must be
// written here so agent.Run's post-Ensure os.Environ() snapshot carries
// OPENCODE_CONFIG.
func writeAutonomousConfig() error {
	path := filepath.Join(os.TempDir(), "opencode-agentbox.json")
	reviewing := strings.TrimSpace(os.Getenv("AGENT_MODE")) == config.ModeReview
	cfg, err := json.Marshal(agentConfig(os.Getenv(config.MCPSocketEnv), reviewing))
	if err != nil {
		return fmt.Errorf("building opencode config: %w", err)
	}
	if err := os.WriteFile(path, cfg, 0o644); err != nil {
		return fmt.Errorf("writing opencode autonomy config: %w", err)
	}
	if err := os.Setenv(opencodeConfigEnv, path); err != nil {
		return fmt.Errorf("setting %s: %w", opencodeConfigEnv, err)
	}
	return nil
}

// autonomousConfig builds the config document: full tool autonomy, plus the
// runner's tool socket registered as a local (stdio) MCP server when one is
// exposed.
//
// The socket is read from the environment rather than *config.Config because
// Ensure — where the config file must be written, so agent.Run's post-Ensure
// os.Environ() snapshot carries OPENCODE_CONFIG — takes no cfg. Same var
// config.Load reads, same approach AllowedHosts already takes here.
//
// opencode's schema puts MCP servers in an `mcp` block keyed by name, where a
// local server is {"type":"local","command":[...]} with command and args in
// one array. Registering nothing when the socket is absent keeps a plain
// agent run free of a dangling bridge.
// A reviewing run denies edit and bash rather than allowing everything. The
// review prompt already says not to touch the tree, but a prompt is a request;
// this is the guarantee. opencode's read, grep, glob and list tools are
// unaffected by these two denials, so the reviewer keeps everything it needs to
// read the change.
func agentConfig(socket string, reviewing bool) map[string]any {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		// Headless runs must never block on a permission prompt — the
		// container sandbox and network allowlist are the real guardrails.
		"permission": "allow",
	}
	if reviewing {
		cfg["permission"] = map[string]any{
			"edit": "deny",
			"bash": "deny",
		}
	}
	socket = strings.TrimSpace(socket)
	if socket == "" || reviewing {
		// A reviewer needs no runner tools, and a tool channel it cannot use
		// is a channel it cannot misuse.
		return cfg
	}
	command, args := agent.BridgeCommand(socket)
	cfg["mcp"] = map[string]any{
		"deployment-io": map[string]any{
			"type":    "local",
			"command": append([]string{command}, args...),
			"enabled": true,
			// Discovery timeout, not execution: opencode uses this when
			// fetching the server's tool list, and a server that misses it
			// has its tools silently not loaded — the agent then runs as
			// though no tool channel existed. The 5s default is a cold-start
			// race here, since the bridge has to exec and the runner has to
			// answer before it expires. Long tool calls (a preview deploy)
			// are unaffected either way; this only buys headroom on startup.
			"timeout": bridgeDiscoveryTimeoutMS,
		},
	}
	return cfg
}

func (d *Driver) Binary() string {
	return "opencode"
}

// BuildArgs assembles the headless `opencode run` invocation:
//
//   - run: non-interactive single-prompt mode (no TUI).
//   - --format json: newline-delimited JSON events on stdout (consumed by the
//     OutputParser). Autonomy comes from OPENCODE_CONFIG (see Ensure), not a
//     flag.
//   - --model provider/model: the model id is provider-prefixed; the runner /
//     kit supply it in that form (see task_models.opencodeModels).
//
// The prompt is the trailing positional arg. opencode runs in cmd.Dir (WORK_DIR,
// set by the orchestrator), so no working-dir flag is needed. opencode has no
// turn-cap / token-budget flag, so those limits are enforced agentbox-side from
// the JSON event stream (see agent.Run's limit watcher).
func (d *Driver) BuildArgs(cfg *config.Config) []string {
	args := []string{"run", "--format", "json"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, cfg.StepPrompt+"\n\n"+trailingInstruction(cfg))
	return args
}

// trailingInstruction picks which contract this run is held to — see the
// claude driver's copy. In review mode the implementer's instruction is not
// appended, so no <verify> or <pr_title> trailer is requested or produced.
func trailingInstruction(cfg *config.Config) string {
	if cfg.Mode == config.ModeReview {
		return review.Instruction(cfg.ReviewPasses)
	}
	return finalMessageInstruction
}

func (d *Driver) DetectVersion() string {
	out, err := exec.Command(d.Binary(), "--version").Output()
	if err != nil {
		return ""
	}
	// `opencode --version` prints a version line; keep the first semver-looking
	// token (starts with a digit), tolerating an optional "opencode " prefix.
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

// NewLogFormatter turns opencode's `run --format json` JSONL into compact
// one-line summaries for the container log, while teeing the unfiltered stream
// to /scratch/agent.log for deep debugging. Non-JSON lines (npm output, proxy
// denies) pass through verbatim. Mirrors the codex driver's formatter.
func (d *Driver) NewLogFormatter(sink io.Writer) io.WriteCloser {
	return newHumanLogFormatter(sink, openRawStreamLog())
}

// Capabilities reports MCP tool support: writeAutonomousConfig registers
// the bridge as a local stdio server in opencode's `mcp` block when the
// runner exposes a socket.
func (d *Driver) Capabilities() agent.Capabilities {
	return agent.Capabilities{MCPTools: true}
}
