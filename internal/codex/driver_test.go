package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
)

func TestRegistered_Codex(t *testing.T) {
	d, err := agent.DriverFor("codex", "")
	if err != nil {
		t.Fatalf("codex should be registered: %v", err)
	}
	if d.Binary() != "codex" {
		t.Errorf("Binary() = %q, want codex", d.Binary())
	}
}

func TestBuildArgs_Minimal(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{StepPrompt: "hello"})

	// Static flags come first in a stable order; the prompt is last.
	wantPrefix := []string{
		"exec",
		"--json",
		"--sandbox", "danger-full-access",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
	}
	for i, w := range wantPrefix {
		if i >= len(args) || args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, argOrMissing(args, i), w)
		}
	}
	if slices.Contains(args, "--model") {
		t.Error("--model should not be present when Model is empty")
	}
	// The prompt is the last arg and carries the user prompt plus the
	// final-message instruction (Codex exec has no system-prompt flag).
	last := args[len(args)-1]
	if !strings.HasPrefix(last, "hello") {
		t.Errorf("last arg should start with the prompt, got %q", last)
	}
	if !strings.Contains(last, "<pr_title>") {
		t.Error("prompt arg should include the final-message instruction")
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{StepPrompt: "hello", Model: "gpt-5.5"})
	assertFollowedBy(t, args, "--model", "gpt-5.5")
	assertFollowedBy(t, args, "--sandbox", "danger-full-access")
	if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Error("--dangerously-bypass-approvals-and-sandbox should be present")
	}
	if !slices.Contains(args, "--skip-git-repo-check") {
		t.Error("--skip-git-repo-check should be present")
	}
}

func TestAllowedHosts_IncludesOpenAI(t *testing.T) {
	hosts := (&Driver{}).AllowedHosts()
	if !slices.Contains(hosts, "api.openai.com") {
		t.Errorf("AllowedHosts should include api.openai.com, got %v", hosts)
	}
}

func argOrMissing(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return "<missing>"
}

func assertFollowedBy(t *testing.T, args []string, flag, val string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) && args[i+1] == val {
				return
			}
			t.Errorf("flag %q not followed by %q (args: %v)", flag, val, args)
			return
		}
	}
	t.Errorf("flag %q not found in args %v", flag, args)
}
