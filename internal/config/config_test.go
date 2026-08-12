package config

import (
	"os"
	"path/filepath"
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
		"CLAUDE_CODE_VERSION",
		"NO_ACTIVITY_TIMEOUT",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_REGION",
		"AGENT_MODE",
		"SESSION_ID",
		"MAX_BUDGET_USD",
		"READ_ONLY",
		"APPEND_SYSTEM_PROMPT_FILE",
	}
	for _, v := range vars {
		t.Setenv(v, want[v])
	}
}

// TestLoadCodexCredentials pins the codex credential contract: OPENAI_API_KEY
// is what the Codex CLI authenticates with; CODEX_API_KEY is a legacy alias
// mapped onto it at startup; neither present fails fast with a clear message
// instead of dying later with a misleading 401 from api.openai.com.
func TestLoadCodexCredentials(t *testing.T) {
	c := &Config{}

	t.Run("openai key alone passes", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-real")
		t.Setenv("CODEX_API_KEY", "")
		if err := c.loadCodexCredentials(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := os.Getenv("OPENAI_API_KEY"); got != "sk-real" {
			t.Errorf("OPENAI_API_KEY = %q, want untouched sk-real", got)
		}
	})

	t.Run("legacy codex key maps onto OPENAI_API_KEY", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("CODEX_API_KEY", "sk-legacy")
		if err := c.loadCodexCredentials(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := os.Getenv("OPENAI_API_KEY"); got != "sk-legacy" {
			t.Errorf("OPENAI_API_KEY = %q, want mapped legacy value", got)
		}
	})

	t.Run("both set keeps the explicit openai key", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-real")
		t.Setenv("CODEX_API_KEY", "sk-legacy")
		if err := c.loadCodexCredentials(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := os.Getenv("OPENAI_API_KEY"); got != "sk-real" {
			t.Errorf("OPENAI_API_KEY = %q, want explicit value kept", got)
		}
	})

	t.Run("neither fails fast naming OPENAI_API_KEY", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("CODEX_API_KEY", "")
		err := c.loadCodexCredentials()
		if err == nil {
			t.Fatal("expected error with no codex credential")
		}
		if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
			t.Errorf("error %q should name OPENAI_API_KEY", err)
		}
	})
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

func TestLoad_SubscriptionOAuth(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":             "do the thing",
		"WORK_DIR":                workDir,
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-test",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Subscription == nil {
		t.Fatal("expected Subscription to be populated")
	}
	if cfg.Subscription.OAuthToken != "sk-ant-oat01-test" {
		t.Errorf("OAuthToken = %q, want %q", cfg.Subscription.OAuthToken, "sk-ant-oat01-test")
	}
	if cfg.AnthropicDirect != nil {
		t.Error("AnthropicDirect should be nil when the subscription OAuth path is set")
	}
	if cfg.Bedrock != nil {
		t.Error("Bedrock should be nil when the subscription OAuth path is set")
	}
}

func TestLoad_SubscriptionConflictsWithAPIKey(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":             "do the thing",
		"WORK_DIR":                workDir,
		"ANTHROPIC_API_KEY":       "sk-ant-test",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-test",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when both API key and OAuth token are set")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should mention the ambiguity: %v", err)
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

func TestLoad_AgentVersionReadFromEnv(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":         "do the thing",
		"WORK_DIR":            workDir,
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"CLAUDE_CODE_VERSION": "2.1.117",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AgentType != "claude-code" {
		t.Errorf("AgentType = %q, want claude-code (default)", cfg.AgentType)
	}
	if cfg.AgentVersion != "2.1.117" {
		t.Errorf("AgentVersion = %q, want 2.1.117", cfg.AgentVersion)
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
	if cfg.NoActivityTimeout != 10*time.Minute {
		t.Errorf("default NoActivityTimeout = %v, want 10m", cfg.NoActivityTimeout)
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

func TestLoad_DefaultModeBatch(t *testing.T) {
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
	if cfg.Mode != ModeBatch {
		t.Errorf("default Mode = %q, want %q", cfg.Mode, ModeBatch)
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        "bogus",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid AGENT_MODE")
	}
	if !strings.Contains(err.Error(), "AGENT_MODE") {
		t.Errorf("error should mention AGENT_MODE: %v", err)
	}
}

func TestLoad_InteractiveModeAllowsEmptyStepPrompt(t *testing.T) {
	// Interactive mode gets its work over the message pipe, not STEP_PROMPT,
	// so an empty STEP_PROMPT must not fail Load the way it does in batch.
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        ModeInteractive,
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("interactive mode should not require STEP_PROMPT: %v", err)
	}
	if cfg.Mode != ModeInteractive {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeInteractive)
	}
	if cfg.StepPrompt != "" {
		t.Errorf("StepPrompt = %q, want empty", cfg.StepPrompt)
	}
}

func TestLoad_InteractiveFields(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        ModeInteractive,
		"SESSION_ID":        "11111111-2222-3333-4444-555555555555",
		"MAX_BUDGET_USD":    "5.00",
		"READ_ONLY":         "1",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("SessionID = %q", cfg.SessionID)
	}
	if cfg.MaxBudgetUSD != "5.00" {
		t.Errorf("MaxBudgetUSD = %q, want 5.00", cfg.MaxBudgetUSD)
	}
	if !cfg.ReadOnly {
		t.Error("ReadOnly = false, want true")
	}
}

func TestLoad_ReadOnlyParsing(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"", false},
		{"nope", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			workDir := t.TempDir()
			setEnv(t, map[string]string{
				"STEP_PROMPT":       "do the thing",
				"WORK_DIR":          workDir,
				"ANTHROPIC_API_KEY": "sk-ant-test",
				"READ_ONLY":         tt.val,
			})
			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ReadOnly != tt.want {
				t.Errorf("READ_ONLY=%q -> ReadOnly=%v, want %v", tt.val, cfg.ReadOnly, tt.want)
			}
		})
	}
}

func TestLoad_AppendSystemPromptFromFile(t *testing.T) {
	workDir := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "system-prompt.txt")
	want := "You are in plan mode. Be read-only."
	if err := os.WriteFile(promptPath, []byte(want), 0o644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	setEnv(t, map[string]string{
		"WORK_DIR":                  workDir,
		"ANTHROPIC_API_KEY":         "sk-ant-test",
		"AGENT_MODE":                ModeInteractive,
		"APPEND_SYSTEM_PROMPT_FILE": promptPath,
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AppendSystemPrompt != want {
		t.Errorf("AppendSystemPrompt = %q, want %q", cfg.AppendSystemPrompt, want)
	}
}

func TestLoad_AppendSystemPromptFileMissing(t *testing.T) {
	workDir := t.TempDir()
	setEnv(t, map[string]string{
		"WORK_DIR":                  workDir,
		"ANTHROPIC_API_KEY":         "sk-ant-test",
		"AGENT_MODE":                ModeInteractive,
		"APPEND_SYSTEM_PROMPT_FILE": "/nonexistent/prompt.txt",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for unreadable APPEND_SYSTEM_PROMPT_FILE")
	}
	if !strings.Contains(err.Error(), "APPEND_SYSTEM_PROMPT_FILE") {
		t.Errorf("error should mention the var: %v", err)
	}
}

// ⚠️ THE CREDENTIAL TABLE AND THE HOST TABLE MUST COVER THE SAME PROVIDERS.
//
// opencodeProviderEnvKey lives here and providerHostFromModel lives in
// internal/opencode, deliberately unshared to avoid an import cycle. Drift is
// silent and breaks differently each way: a provider with a credential entry
// but no host gets its egress denied by the proxy; one with a host but no
// credential entry skips the fail-fast check and 401s mid-run instead.
//
// Neither package can see the other's unexported table, so each pins the set
// it knows. Counterpart: TestProviderHosts_CoverTheExpectedSet.
func TestOpencodeProviderEnvKey_CoversTheExpectedSet(t *testing.T) {
	// Keep in step with providerHostFromModel in internal/opencode.
	want := map[string]string{
		"anthropic":  "ANTHROPIC_API_KEY",
		"openai":     "OPENAI_API_KEY",
		"openrouter": "OPENROUTER_API_KEY",
		// opencode's provider id, not our catalogue key ("novita").
		"novita-ai": "NOVITA_API_KEY",
		"google":    "GEMINI_API_KEY",
		"groq":      "GROQ_API_KEY",
		"xai":       "XAI_API_KEY",
		"deepseek":  "DEEPSEEK_API_KEY",
		"mistral":   "MISTRAL_API_KEY",
	}
	for provider, key := range want {
		if got := opencodeProviderEnvKey(provider); got != key {
			t.Errorf("opencodeProviderEnvKey(%q) = %q, want %q", provider, got, key)
		}
	}
	// Unknown providers stay lenient: opencode autodetects whatever key is in
	// the env and 401s at runtime, which beats refusing a provider we simply
	// have not tabulated.
	if got := opencodeProviderEnvKey("some-future-provider"); got != "" {
		t.Errorf("unknown provider returned %q; the lenient path must be preserved", got)
	}
}
