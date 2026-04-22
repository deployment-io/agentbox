// Package config loads and validates the agentbox environment contract.
//
// See docs/CONTRACT.md for the full input spec.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config captures the validated inputs for one agentbox run.
type Config struct {
	StepPrompt           string
	WorkDir              string
	PreviousStepsSummary string
	Model                string
	MaxTurns             string
	AgentType            string

	// Exactly one of AnthropicDirect or Bedrock is populated.
	AnthropicDirect *AnthropicDirectCreds
	Bedrock         *BedrockCreds
}

// AnthropicDirectCreds holds the credentials for the Anthropic Direct path.
type AnthropicDirectCreds struct {
	APIKey string
}

// BedrockCreds holds the credentials for the AWS Bedrock path.
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

	if c.AgentType != "claude-code" {
		return nil, fmt.Errorf("AGENT_TYPE %q is not supported in v1 (only claude-code)", c.AgentType)
	}

	if err := c.loadCredentials(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Config) loadCredentials() error {
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
