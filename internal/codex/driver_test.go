package codex

import (
	"context"
	"os"
	"path/filepath"
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

// stubCodexOnPath installs a fake `codex` executable that records its argv
// and stdin, so the login wiring can be asserted without the real CLI.
// Returns the capture file paths.
func stubCodexOnPath(t *testing.T) (argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdinFile = filepath.Join(dir, "stdin")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\necho Successfully logged in\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile, stdinFile
}

// TestEnsure_LoginWithAPIKey pins the auth contract: the Codex CLI does not
// read OPENAI_API_KEY from the env for request auth (a valid key in the env
// alone still 401s on the app-server's responses websocket — verified on
// 0.136.0), so Ensure must register the key via `codex login --with-api-key`
// with the key on stdin — including when the CLI is already installed.
func TestEnsure_LoginWithAPIKey(t *testing.T) {
	argsFile, stdinFile := stubCodexOnPath(t)
	t.Setenv("OPENAI_API_KEY", "sk-test-abc123")

	if err := (&Driver{}).Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("stub codex was not invoked: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "login --with-api-key" {
		t.Errorf("codex argv = %q, want %q", got, "login --with-api-key")
	}
	stdin, _ := os.ReadFile(stdinFile)
	if string(stdin) != "sk-test-abc123" {
		t.Errorf("key on stdin = %q, want the env key", stdin)
	}
}

func TestEnsure_NoKeyNoLogin(t *testing.T) {
	argsFile, _ := stubCodexOnPath(t)
	t.Setenv("OPENAI_API_KEY", "")

	if err := (&Driver{}).Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Errorf("codex should not be invoked when no key is set")
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
	// Non-essential network calls are disabled via -c overrides.
	for _, k := range quietStartupOverrides {
		if !slices.Contains(args, k) {
			t.Errorf("expected -c %q to disable non-essential calls", k)
		}
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

// quietStartupOverrides is every -c key whose job is to keep Codex off the
// hosts the egress proxy denies. Named once so both the implement-mode and the
// review-mode assertions below check the same set: a key that only reaches one
// of the two modes leaves the other logging denials.
var quietStartupOverrides = []string{
	"features.plugins=false",
	"check_for_update_on_startup=false",
	"analytics.enabled=false",
	"otel.exporter=none",
	"otel.metrics_exporter=none",
}

// features.plugins=false is the key that earns its place here. On the pinned
// 0.147.0 the other four are all still accepted (`codex exec --strict-config`
// takes every one) and still do work — dropping the otel/analytics pair brings
// back a periodic POST to ab.chatgpt.com/otlp/v1/metrics — but NONE of them
// touches the startup denials that were actually observed. Those are the
// curated plugin sync, which `codex exec` runs while creating the thread:
// chatgpt.com/backend-api/plugins/featured, then git ls-remote on
// github.com/openai/plugins.git, then api.github.com/repos/openai/plugins, then
// chatgpt.com/backend-api/plugins/export/curated as each is refused in turn.
// Four requests, three denied hosts, one feature flag.
//
// Asserted in REVIEW mode too: a review run takes a different branch through
// BuildArgs (sandbox flags, no MCP, prompt on stdin), and a review that logs
// the same three denials is the run this was reported from.
func TestBuildArgsQuietsTheStartupCallsInEveryMode(t *testing.T) {
	d := &Driver{}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"implement", &config.Config{StepPrompt: "hello"}},
		{"review", &config.Config{Mode: config.ModeReview, StepPrompt: "the diff", ReviewPasses: []string{"security"}}},
		{"review on read-only mounts", &config.Config{Mode: config.ModeReview, StepPrompt: "the diff", ReviewReadOnlyMounts: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := d.BuildArgs(tc.cfg)
			for _, key := range quietStartupOverrides {
				i := slices.Index(args, key)
				if i < 0 {
					t.Errorf("-c %q is missing: %v", key, args)
					continue
				}
				if i == 0 || args[i-1] != "-c" {
					t.Errorf("%q is not preceded by -c: %v", key, args)
				}
			}
		})
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
