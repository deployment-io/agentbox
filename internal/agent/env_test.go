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

// The review inputs are agentbox's contract too, and they carry exactly the
// content that breaks Codex's shell-environment snapshot: REVIEW_SPEC is
// free-form prose or JSON with embedded quotes and newlines, and
// REVIEW_BASE_COMMITS is a JSON object. The agent receives the diff, the spec
// and the pass list folded into the prompt it is given, so forwarding them is
// pointless as well as dangerous.
func TestBuildEnv_StripsReviewInputVars(t *testing.T) {
	t.Setenv("REVIEW_SPEC", "{\"title\":\"Add \\\"login\\\"\",\n\"goal\":\"x\"}")
	t.Setenv("REVIEW_PASSES", "security,correctness")
	t.Setenv("REVIEW_BASE_COMMITS", `{"0-acme/api":"abc123"}`)
	t.Setenv("REVIEW_ROUND", "2")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	env := buildEnv()

	for _, key := range []string{"REVIEW_SPEC", "REVIEW_PASSES", "REVIEW_BASE_COMMITS", "REVIEW_ROUND"} {
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				t.Errorf("%s must be stripped from the agent subprocess env", key)
			}
		}
	}
	if !envContains(env, "ANTHROPIC_API_KEY=sk-ant-test") {
		t.Error("the credential must still be forwarded to the agent")
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
