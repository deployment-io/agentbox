package agent

import (
	"slices"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func TestBuildArgs_Minimal(t *testing.T) {
	args := buildArgs(&config.Config{StepPrompt: "hello"})

	wantPrefix := []string{"-p", "hello", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	for i, w := range wantPrefix {
		if i >= len(args) || args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, getOr(args, i), w)
		}
	}

	if slices.Contains(args, "--model") {
		t.Error("--model should not be present when Model is empty")
	}
	if slices.Contains(args, "--max-turns") {
		t.Error("--max-turns should not be present when MaxTurns is empty")
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	args := buildArgs(&config.Config{StepPrompt: "hello", Model: "opus"})

	if !slices.Contains(args, "--model") {
		t.Fatal("--model flag missing")
	}
	for i, a := range args {
		if a == "--model" {
			if i+1 >= len(args) || args[i+1] != "opus" {
				t.Errorf("--model value = %q, want opus", getOr(args, i+1))
			}
			break
		}
	}
}

func TestBuildArgs_WithMaxTurns(t *testing.T) {
	args := buildArgs(&config.Config{StepPrompt: "hello", MaxTurns: "50"})

	if !slices.Contains(args, "--max-turns") {
		t.Fatal("--max-turns flag missing")
	}
	for i, a := range args {
		if a == "--max-turns" {
			if i+1 >= len(args) || args[i+1] != "50" {
				t.Errorf("--max-turns value = %q, want 50", getOr(args, i+1))
			}
			break
		}
	}
}

func TestBuildArgs_PromptIsLiteral(t *testing.T) {
	// A prompt that looks like flags should be passed literally as a
	// single positional string, not parsed as flags.
	tricky := "--not-a-flag"
	args := buildArgs(&config.Config{StepPrompt: tricky})

	// Verify the prompt appears right after -p as a single argument.
	for i, a := range args {
		if a == "-p" {
			if i+1 >= len(args) || args[i+1] != tricky {
				t.Errorf("prompt after -p = %q, want %q", getOr(args, i+1), tricky)
			}
			return
		}
	}
	t.Error("-p flag missing")
}

func getOr(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return "<out-of-range>"
	}
	return s[i]
}
