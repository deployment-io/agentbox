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
	}

	if c.StepPrompt == "" {
		return nil, fmt.Errorf("STEP_PROMPT is required")
	}

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

// loadCodexCredentials requires CODEX_API_KEY — the key `codex exec` reads
// for headless runs. OpenAI direct only in v1 (no provider dispatch).
func (c *Config) loadCodexCredentials() error {
	if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) == "" {
		return fmt.Errorf("CODEX_API_KEY is required for codex")
	}
	return nil
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
