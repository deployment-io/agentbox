package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv clears the env vars we care about and sets them to the values
// in want. Relies on t.Setenv for automatic cleanup at test end.
func setEnv(t *testing.T, want map[string]string) {
	t.Helper()
	vars := []string{
		"STEP_PROMPT",
		"WORK_DIR",
		"PREVIOUS_STEPS_SUMMARY",
		"MODEL",
		"MAX_TURNS",
		"AGENT_TYPE",
		"NO_ACTIVITY_TIMEOUT",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_REGION",
	}
	for _, v := range vars {
		t.Setenv(v, want[v])
	}
}

func TestLoad_MissingStepPrompt(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":          t.TempDir(),
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing STEP_PROMPT")
	}
	if !strings.Contains(err.Error(), "STEP_PROMPT") {
		t.Errorf("error should mention STEP_PROMPT: %v", err)
	}
}

func TestLoad_MissingWorkDir(t *testing.T) {
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          "/nonexistent/path",
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for nonexistent WORK_DIR")
	}
	if !strings.Contains(err.Error(), "WORK_DIR") {
		t.Errorf("error should mention WORK_DIR: %v", err)
	}
}

func TestLoad_AnthropicDirect(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicDirect == nil {
		t.Fatal("expected AnthropicDirect to be populated")
	}
	if cfg.AnthropicDirect.APIKey != "sk-ant-test" {
		t.Errorf("APIKey = %q, want %q", cfg.AnthropicDirect.APIKey, "sk-ant-test")
	}
	if cfg.Bedrock != nil {
		t.Error("Bedrock should be nil when Anthropic Direct is set")
	}
	if cfg.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want %q", cfg.WorkDir, workDir)
	}
	if cfg.AgentType != "claude-code" {
		t.Errorf("default AgentType = %q, want %q", cfg.AgentType, "claude-code")
	}
}

func TestLoad_Bedrock(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":             "do the thing",
		"WORK_DIR":                workDir,
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_ACCESS_KEY_ID":       "AKIAFAKE",
		"AWS_SECRET_ACCESS_KEY":   "secretfake",
		"AWS_SESSION_TOKEN":       "sessionfake",
		"AWS_REGION":              "us-west-2",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Bedrock == nil {
		t.Fatal("expected Bedrock to be populated")
	}
	if cfg.AnthropicDirect != nil {
		t.Error("AnthropicDirect should be nil when Bedrock is set")
	}
	if cfg.Bedrock.AccessKeyID != "AKIAFAKE" {
		t.Errorf("AccessKeyID = %q, want %q", cfg.Bedrock.AccessKeyID, "AKIAFAKE")
	}
	if cfg.Bedrock.Region != "us-west-2" {
		t.Errorf("Region = %q, want %q", cfg.Bedrock.Region, "us-west-2")
	}
	if cfg.Bedrock.SessionToken != "sessionfake" {
		t.Errorf("SessionToken = %q, want %q", cfg.Bedrock.SessionToken, "sessionfake")
	}
}

func TestLoad_BedrockWithoutSessionToken(t *testing.T) {
	// Permanent AWS credentials (no session token) should also work.
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":             "do the thing",
		"WORK_DIR":                workDir,
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_ACCESS_KEY_ID":       "AKIAFAKE",
		"AWS_SECRET_ACCESS_KEY":   "secretfake",
		"AWS_REGION":              "us-east-1",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Bedrock == nil {
		t.Fatal("expected Bedrock to be populated")
	}
	if cfg.Bedrock.SessionToken != "" {
		t.Errorf("SessionToken should be empty, got %q", cfg.Bedrock.SessionToken)
	}
}

func TestLoad_BothCredentialPaths(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":             "do the thing",
		"WORK_DIR":                workDir,
		"ANTHROPIC_API_KEY":       "sk-ant-test",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_ACCESS_KEY_ID":       "AKIAFAKE",
		"AWS_SECRET_ACCESS_KEY":   "secretfake",
		"AWS_REGION":              "us-west-2",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when both credential paths are set")
	}
	if !strings.Contains(err.Error(), "both") && !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should mention the ambiguity: %v", err)
	}
}

func TestLoad_NeitherCredentialPath(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT": "do the thing",
		"WORK_DIR":    workDir,
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when no credential path is set")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error should mention credentials: %v", err)
	}
}

func TestLoad_BedrockMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		missing string
		env     map[string]string
	}{
		{
			name:    "missing AWS_ACCESS_KEY_ID",
			missing: "AWS_ACCESS_KEY_ID",
			env: map[string]string{
				"CLAUDE_CODE_USE_BEDROCK": "1",
				"AWS_SECRET_ACCESS_KEY":   "secretfake",
				"AWS_REGION":              "us-west-2",
			},
		},
		{
			name:    "missing AWS_SECRET_ACCESS_KEY",
			missing: "AWS_SECRET_ACCESS_KEY",
			env: map[string]string{
				"CLAUDE_CODE_USE_BEDROCK": "1",
				"AWS_ACCESS_KEY_ID":       "AKIAFAKE",
				"AWS_REGION":              "us-west-2",
			},
		},
		{
			name:    "missing AWS_REGION",
			missing: "AWS_REGION",
			env: map[string]string{
				"CLAUDE_CODE_USE_BEDROCK": "1",
				"AWS_ACCESS_KEY_ID":       "AKIAFAKE",
				"AWS_SECRET_ACCESS_KEY":   "secretfake",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			tt.env["STEP_PROMPT"] = "do the thing"
			tt.env["WORK_DIR"] = workDir
			setEnv(t, tt.env)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for missing %s", tt.missing)
			}
		})
	}
}

func TestLoad_WhitespaceStepPromptRejected(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "   \t\n  ",
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for whitespace-only STEP_PROMPT")
	}
}

func TestLoad_DefaultWorkDir(t *testing.T) {
	// If WORK_DIR is unset, code falls back to /work. Since /work won't
	// exist in the test env, Load should return an error about WORK_DIR.
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error because default /work doesn't exist in test env")
	}
	if !strings.Contains(err.Error(), "/work") {
		t.Errorf("error should mention /work (the default): %v", err)
	}
}

func TestLoad_OptionalFieldsPassThrough(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":            "do the thing",
		"WORK_DIR":               workDir,
		"ANTHROPIC_API_KEY":      "sk-ant-test",
		"PREVIOUS_STEPS_SUMMARY": "Step 1 was done.",
		"MODEL":                  "opus",
		"MAX_TURNS":              "50",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PreviousStepsSummary != "Step 1 was done." {
		t.Errorf("PreviousStepsSummary = %q", cfg.PreviousStepsSummary)
	}
	if cfg.Model != "opus" {
		t.Errorf("Model = %q, want %q", cfg.Model, "opus")
	}
	if cfg.MaxTurns != "50" {
		t.Errorf("MaxTurns = %q, want %q", cfg.MaxTurns, "50")
	}
}

func TestLoad_UnsupportedAgentType(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_TYPE":        "codex",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for unsupported AGENT_TYPE")
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Errorf("error should mention the unsupported type: %v", err)
	}
}

func TestLoad_NoActivityTimeoutDefault(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NoActivityTimeout != 20*time.Minute {
		t.Errorf("default NoActivityTimeout = %v, want 20m", cfg.NoActivityTimeout)
	}
}

func TestLoad_NoActivityTimeoutCustom(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":         "do the thing",
		"WORK_DIR":            workDir,
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"NO_ACTIVITY_TIMEOUT": "5m30s",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NoActivityTimeout != 5*time.Minute+30*time.Second {
		t.Errorf("NoActivityTimeout = %v, want 5m30s", cfg.NoActivityTimeout)
	}
}

func TestLoad_NoActivityTimeoutDisabled(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":         "do the thing",
		"WORK_DIR":            workDir,
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"NO_ACTIVITY_TIMEOUT": "0",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NoActivityTimeout != 0 {
		t.Errorf("NoActivityTimeout = %v, want 0 (disabled)", cfg.NoActivityTimeout)
	}
}

func TestLoad_NoActivityTimeoutInvalid(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":         "do the thing",
		"WORK_DIR":            workDir,
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"NO_ACTIVITY_TIMEOUT": "not-a-duration",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid NO_ACTIVITY_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "NO_ACTIVITY_TIMEOUT") {
		t.Errorf("error should mention the var: %v", err)
	}
}

func TestLoad_NoActivityTimeoutNegative(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":         "do the thing",
		"WORK_DIR":            workDir,
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"NO_ACTIVITY_TIMEOUT": "-5m",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative NO_ACTIVITY_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("error should mention the sign constraint: %v", err)
	}
}
