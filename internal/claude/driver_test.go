package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

func TestRegistered_ClaudeCode(t *testing.T) {
	d, err := agent.DriverFor("claude-code", "2.1.117")
	if err != nil {
		t.Fatalf("claude-code should be registered: %v", err)
	}
	if d.Binary() != "claude" {
		t.Errorf("Binary() = %q, want claude", d.Binary())
	}
}

func TestBuildArgs_Minimal(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{StepPrompt: "hello"})

	// -p must come first (Claude Code's prompt flag), then the
	// rest of the static flags in a stable order. --append-system-prompt
	// is included unconditionally (Bug 2 fix); see
	// TestBuildArgs_AppendsSystemPromptInstruction for its specific
	// assertion.
	wantPrefix := []string{
		"-p", "hello",
		"--append-system-prompt", finalMessageInstruction,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	for i, w := range wantPrefix {
		if i >= len(args) || args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, argOrMissing(args, i), w)
		}
	}
	if slices.Contains(args, "--model") {
		t.Error("--model should not be present when Model is empty")
	}
	if slices.Contains(args, "--max-turns") {
		t.Error("--max-turns should not be present when MaxTurns is empty")
	}
}

func TestBuildArgs_WithOverrides(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		StepPrompt: "hello",
		Model:      "opus",
		MaxTurns:   "50",
	})

	assertFollowedBy(t, args, "--model", "opus")
	assertFollowedBy(t, args, "--max-turns", "50")
}

func TestBuildArgs_PromptIsLiteral(t *testing.T) {
	tricky := "--not-a-flag"
	d := &Driver{}
	args := d.BuildArgs(&config.Config{StepPrompt: tricky})

	for i, a := range args {
		if a == "-p" {
			if i+1 >= len(args) || args[i+1] != tricky {
				t.Errorf("prompt after -p = %q, want %q", argOrMissing(args, i+1), tricky)
			}
			return
		}
	}
	t.Error("-p flag missing")
}

// TestBuildArgs_AppendsSystemPromptInstruction pins the Bug 2 fix: the
// agent's final-message format instruction (incl. the <pr_title> tag
// convention) is appended to Claude Code's system prompt so the parser
// has a stable trailer to extract. Without this, the agent emits prose
// only and the runner has no clean PR title to use.
func TestBuildArgs_AppendsSystemPromptInstruction(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{StepPrompt: "hello"})

	assertFollowedBy(t, args, "--append-system-prompt", finalMessageInstruction)
	if !strings.Contains(finalMessageInstruction, "<pr_title>") {
		t.Error("finalMessageInstruction must mention the <pr_title> tag the parser expects")
	}
}

func assertFollowedBy(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) || args[i+1] != want {
				t.Errorf("%s value = %q, want %q", flag, argOrMissing(args, i+1), want)
			}
			return
		}
	}
	t.Errorf("%s flag missing", flag)
}

func argOrMissing(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return "<out-of-range>"
	}
	return s[i]
}
