// Package config loads and validates the agentbox environment contract.
//
// See docs/CONTRACT.md for the full input spec.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultNoActivityTimeout = 10 * time.Minute

// Agent execution modes, selected via the AGENT_MODE env var.
const (
	// ModeBatch is the default fire-and-forget mode: one
	// `claude -p "<STEP_PROMPT>"`, one /result.json, exit.
	ModeBatch = "batch"
	// ModeInteractive keeps the container alive for a long-lived,
	// bidirectional stream-json session driven by user messages over a
	// pipe (see internal/agent/interactive.go). Used by repo-aware chat.
	ModeInteractive = "interactive"
)

// Config captures the validated inputs for one agentbox run.
type Config struct {
	StepPrompt           string
	WorkDir              string
	PreviousStepsSummary string
	Model                string
	MaxTurns             string
	AgentType            string
	AgentVersion         string

	// TokenBudget is the cumulative input+output token cap for one run, 0
	// when unset (uncapped). Enforced agentbox-side by the Run loop's
	// limit watcher for agents whose CLI lacks a native budget flag (e.g.
	// Codex); Claude Code reports usage only at the end, so the watcher
	// never preempts it.
	TokenBudget int

	// Mode is batch (default) or interactive — see the Mode* constants.
	// From AGENT_MODE.
	Mode string

	// SessionID, when set, is forwarded to the agent as a stable session
	// identifier (claude --session-id) so the transcript persists on disk
	// and can be resumed after a container restart. From SESSION_ID.
	SessionID string

	// MaxBudgetUSD caps total spend for the run (claude --max-budget-usd).
	// Empty = uncapped. From MAX_BUDGET_USD.
	MaxBudgetUSD string

	// ReadOnly restricts the agent to read-only investigation. The driver
	// builds a tool allowlist and deliberately omits
	// --dangerously-skip-permissions, so the allowlist is enforced rather
	// than bypassed. From READ_ONLY.
	ReadOnly bool

	// AppendSystemPrompt is extra text appended to the agent's default
	// system prompt (claude --append-system-prompt). Loaded from the file
	// named by APPEND_SYSTEM_PROMPT_FILE — the CLI has no
	// --append-system-prompt-file flag, so agentbox reads the file and
	// passes the contents inline. Empty = none.
	AppendSystemPrompt string

	// MCPSocket is the container path of the runner's per-task MCP tool socket
	// (MCP_TOOL_RPC_SOCKET). Set for the agent phase when the runner exposes
	// runner-executed tools; empty = no tool channel. The claude driver points
	// the agent's MCP client at it via --mcp-config + the `mcp-bridge`
	// subcommand. Credentials stay in the runner; only intent crosses the socket.
	MCPSocket string

	// NoActivityTimeout is zero when the detector is disabled.
	NoActivityTimeout time.Duration

	// Exactly one of AnthropicDirect or Bedrock is populated.
	AnthropicDirect *AnthropicDirectCreds
	Bedrock         *BedrockCreds
}

type AnthropicDirectCreds struct {
	APIKey string
}

type BedrockCreds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
}

// Load reads environment variables and validates the contract.
// Returns an error if required vars are missing or if the credential
// path is ambiguous (both set or neither set).
func Load() (*Config, error) {
	c := &Config{
		StepPrompt:           strings.TrimSpace(os.Getenv("STEP_PROMPT")),
		WorkDir:              envOr("WORK_DIR", "/work"),
		PreviousStepsSummary: os.Getenv("PREVIOUS_STEPS_SUMMARY"),
		Model:                os.Getenv("MODEL"),
		MaxTurns:             os.Getenv("MAX_TURNS"),
		AgentType:            envOr("AGENT_TYPE", "claude-code"),
		Mode:                 envOr("AGENT_MODE", ModeBatch),
		SessionID:            strings.TrimSpace(os.Getenv("SESSION_ID")),
		MaxBudgetUSD:         strings.TrimSpace(os.Getenv("MAX_BUDGET_USD")),
		ReadOnly:             parseBoolEnv("READ_ONLY"),
		MCPSocket:            strings.TrimSpace(os.Getenv("MCP_TOOL_RPC_SOCKET")),
	}

	switch c.Mode {
	case ModeBatch, ModeInteractive:
	default:
		return nil, fmt.Errorf("invalid AGENT_MODE %q: must be %q or %q", c.Mode, ModeBatch, ModeInteractive)
	}

	// STEP_PROMPT is the batch-mode work item. Interactive mode receives
	// user turns over the message pipe, so it is not required there.
	if c.Mode == ModeBatch && c.StepPrompt == "" {
		return nil, fmt.Errorf("STEP_PROMPT is required")
	}

	appendPrompt, err := loadAppendSystemPrompt()
	if err != nil {
		return nil, err
	}
	c.AppendSystemPrompt = appendPrompt

	if _, err := os.Stat(c.WorkDir); err != nil {
		return nil, fmt.Errorf("WORK_DIR %q is not accessible: %w", c.WorkDir, err)
	}

	c.AgentVersion = agentVersionForType(c.AgentType)

	timeout, err := parseNoActivityTimeout(os.Getenv("NO_ACTIVITY_TIMEOUT"))
	if err != nil {
		return nil, err
	}
	c.NoActivityTimeout = timeout

	// TOKEN_BUDGET is an optional integer cap; absent / malformed / negative
	// all resolve to 0 (uncapped). Strict validation isn't worth a hard
	// failure here — the limit watcher simply doesn't engage at 0.
	if tb, convErr := strconv.Atoi(os.Getenv("TOKEN_BUDGET")); convErr == nil && tb > 0 {
		c.TokenBudget = tb
	}

	if err := c.loadCredentials(); err != nil {
		return nil, err
	}

	return c, nil
}

// agentVersionForType reads the env var that holds the pinned version
// for the given agent. Unknown types return "" (the Driver lookup
// will reject them later with a clearer error).
func agentVersionForType(agentType string) string {
	switch agentType {
	case "claude-code":
		return os.Getenv("CLAUDE_CODE_VERSION")
	case "codex":
		return os.Getenv("CODEX_VERSION")
	}
	return ""
}

// parseNoActivityTimeout returns the default for "", zero for "0", or
// a parsed Go duration. Negatives and non-durations are errors.
func parseNoActivityTimeout(v string) (time.Duration, error) {
	if v == "" {
		return defaultNoActivityTimeout, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid NO_ACTIVITY_TIMEOUT %q: %w", v, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("NO_ACTIVITY_TIMEOUT must be non-negative, got %s", d)
	}
	return d, nil
}

// loadCredentials validates that the credentials the chosen agent needs
// are present, failing fast at startup. The agent CLI itself reads its key
// from the inherited process env (buildEnv forwards os.Environ); this is a
// presence check, not a hand-off.
func (c *Config) loadCredentials() error {
	if c.AgentType == "codex" {
		return c.loadCodexCredentials()
	}
	return c.loadClaudeCredentials()
}

// loadCodexCredentials requires OPENAI_API_KEY — the key the Codex CLI
// actually authenticates with; without it codex falls back to interactive
// ChatGPT login (blocked by the proxy) and 401s on api.openai.com.
// CODEX_API_KEY — the original contract name, which nothing reads — is
// accepted as a legacy alias and mapped onto OPENAI_API_KEY when only it is
// set, so control planes that predate the rename keep authenticating.
// OpenAI direct only in v1 (no provider dispatch).
func (c *Config) loadCodexCredentials() error {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return nil
	}
	if legacy := strings.TrimSpace(os.Getenv("CODEX_API_KEY")); legacy != "" {
		if err := os.Setenv("OPENAI_API_KEY", legacy); err != nil {
			return fmt.Errorf("error mapping CODEX_API_KEY to OPENAI_API_KEY: %w", err)
		}
		return nil
	}
	return fmt.Errorf("OPENAI_API_KEY is required for codex (CODEX_API_KEY is accepted as a legacy alias)")
}

// loadClaudeCredentials validates the Anthropic Direct or Bedrock path
// (exactly one must be set).
func (c *Config) loadClaudeCredentials() error {
	anthropicKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	bedrockFlag := os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1"

	if anthropicKey != "" && bedrockFlag {
		return fmt.Errorf("both ANTHROPIC_API_KEY and CLAUDE_CODE_USE_BEDROCK=1 are set; specify exactly one credential path")
	}

	if anthropicKey != "" {
		c.AnthropicDirect = &AnthropicDirectCreds{APIKey: anthropicKey}
		return nil
	}

	if bedrockFlag {
		creds := &BedrockCreds{
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			Region:          os.Getenv("AWS_REGION"),
		}
		if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.Region == "" {
			return fmt.Errorf("CLAUDE_CODE_USE_BEDROCK=1 requires AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and AWS_REGION")
		}
		c.Bedrock = creds
		return nil
	}

	return fmt.Errorf("no credentials provided: set ANTHROPIC_API_KEY or CLAUDE_CODE_USE_BEDROCK=1 plus AWS credentials")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv reports whether the named env var holds a truthy value
// ("1", "true", or "yes", case-insensitive). Anything else, including
// unset, is false.
func parseBoolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// loadAppendSystemPrompt reads the file named by APPEND_SYSTEM_PROMPT_FILE
// and returns its contents, to be passed inline via the agent's
// --append-system-prompt flag (the CLI has no --append-system-prompt-file
// variant). Returns "" when the env var is unset; errors when it is set
// but the file cannot be read.
func loadAppendSystemPrompt() (string, error) {
	path := strings.TrimSpace(os.Getenv("APPEND_SYSTEM_PROMPT_FILE"))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("APPEND_SYSTEM_PROMPT_FILE %q is not readable: %w", path, err)
	}
	return string(b), nil
}
