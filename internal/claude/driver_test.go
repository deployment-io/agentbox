package claude

import (
	"slices"
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

	wantPrefix := []string{"-p", "hello", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
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
