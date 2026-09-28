package agent_test

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/claude"
	"github.com/deployment-io/agentbox/internal/config"
)

// The reviewer's permission to run the build hangs on ONE fact — that the
// repositories really are read-only — and that fact is established by the
// write probe, not by the environment variable. Run.go runs the probe before
// it reaches BuildArgs precisely so the driver reads a verdict rather than a
// claim, and these pin the two ends of that wire together. Tested here because
// this is the only package that can see both: internal/claude imports
// internal/agent, so the dependency cannot run the other way inside the
// package itself.

func reviewCfg(t *testing.T, readOnly bool) (*config.Config, string) {
	t.Helper()
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Mode:                 config.ModeReview,
		WorkDir:              workDir,
		StepPrompt:           "the diff and the spec",
		ReviewPasses:         []string{"security"},
		ReviewReadOnlyMounts: readOnly,
	}, repo
}

func allowedTools(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	args := claude.NewDriver("").BuildArgs(cfg)
	i := slices.Index(args, "--allowedTools")
	if i < 0 {
		t.Fatalf("a review run has no --allowedTools allowlist: %v", args)
	}
	return args[i+1:]
}

// A probe that finds a writable repository withdraws the claim, and the
// reviewer is held to the read-only allowlist — the state a stale runner, a
// late-added repository or a missed submodule lands in. A reviewer that could
// run any command in a tree it can write is a reviewer that can edit the change
// it was asked to judge.
func TestAFailedProbeKeepsTheReviewersAllowlist(t *testing.T) {
	cfg, _ := reviewCfg(t, true)
	agent.VerifyReadOnlyMountsForTest(cfg, io.Discard)
	if cfg.ReviewReadOnlyMounts {
		t.Fatal("the probe kept a claim although the repository accepted a write")
	}
	for _, tool := range allowedTools(t, cfg) {
		if tool == "Bash" {
			t.Errorf("a writable repository still got an unrestricted Bash: %v", allowedTools(t, cfg))
		}
	}
}

// A probe that cannot write anywhere lets the claim stand, and the reviewer
// gets the plain Bash it needs to run the build and tests.
func TestASurvivingProbeGrantsTheReviewerBash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so a read-only mount cannot be simulated with chmod")
	}
	cfg, repo := reviewCfg(t, true)
	if err := os.Chmod(repo, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repo, 0o755) })

	agent.VerifyReadOnlyMountsForTest(cfg, io.Discard)
	if !cfg.ReviewReadOnlyMounts {
		t.Fatal("the probe withdrew a claim although no repository accepted a write")
	}
	tools := allowedTools(t, cfg)
	if !slices.Contains(tools, "Bash") {
		t.Errorf("verified read-only mounts did not grant Bash: %v", tools)
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool, "Edit") || strings.HasPrefix(tool, "Write") || strings.HasPrefix(tool, "NotebookEdit") {
			t.Errorf("verified read-only mounts granted the writing tool %q", tool)
		}
	}
}

// And with no claim at all — an older runner — the probe does nothing and the
// allowlist stands, whatever the mounts happen to be.
func TestNoClaimKeepsTheReviewersAllowlist(t *testing.T) {
	cfg, _ := reviewCfg(t, false)
	agent.VerifyReadOnlyMountsForTest(cfg, io.Discard)
	if slices.Contains(allowedTools(t, cfg), "Bash") {
		t.Errorf("an absent claim granted an unrestricted Bash: %v", allowedTools(t, cfg))
	}
}
