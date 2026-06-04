package agent

import (
	"strings"
	"testing"
)

// buildEnv must strip agentbox's own input-contract vars (consumed by
// config.Load, not the agent CLI) from the subprocess env. STEP_PROMPT in
// particular carries quotes/newlines that break Codex's shell-env snapshot.
func TestBuildEnv_StripsAgentboxInputVars(t *testing.T) {
	t.Setenv("STEP_PROMPT", `Add a /health route returning {"status":"ok"}`)
	t.Setenv("PREVIOUS_STEPS_SUMMARY", "summary of earlier steps")
	t.Setenv("CODEX_API_KEY", "sk-test-123")
	t.Setenv("GOMODCACHE", "/cache/mod")

	env := buildEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "STEP_PROMPT=") {
			t.Error("STEP_PROMPT must be stripped from the agent subprocess env")
		}
		if strings.HasPrefix(kv, "PREVIOUS_STEPS_SUMMARY=") {
			t.Error("PREVIOUS_STEPS_SUMMARY must be stripped from the agent subprocess env")
		}
	}

	// Credentials and toolchain/cache vars the agent needs must survive.
	if !envContains(env, "CODEX_API_KEY=sk-test-123") {
		t.Error("CODEX_API_KEY must be forwarded to the agent")
	}
	if !envContains(env, "GOMODCACHE=/cache/mod") {
		t.Error("GOMODCACHE must be forwarded to the agent")
	}
}

func envContains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
