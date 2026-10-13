package claude

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func qaConfig() *config.Config {
	return &config.Config{
		Mode:           config.ModeQA,
		StepPrompt:     "the QA prompt",
		QASpec:         `{"Acceptance":["the home page loads"]}`,
		QARepositories: []string{"0-acme/website"},
		MCPSocket:      "/run/agentbox/tool-rpc.sock",
		MaxTurns:       "30",
	}
}

// QA mode: the preview tools on, no edits, no shell.
func TestBuildArgsInQAMode(t *testing.T) {
	cfg := qaConfig()
	args := (&Driver{}).BuildArgs(cfg)

	if args[0] != "-p" || args[1] == cfg.StepPrompt {
		t.Errorf("the QA prompt must go on stdin, not in the args: %q", args[:2])
	}
	if got := (&Driver{}).Stdin(cfg); got != cfg.StepPrompt {
		t.Errorf("Stdin = %q, want the QA prompt", got)
	}
	for _, a := range args {
		if a == "--dangerously-skip-permissions" {
			t.Error("qa mode bypasses permissions")
		}
	}
	if i := indexOf(args, "--mcp-config"); i < 0 || i+1 >= len(args) || !strings.Contains(args[i+1], `"deployment-io"`) || !strings.Contains(args[i+1], "mcp-bridge") {
		t.Errorf("qa mode does not wire the bridge: %q", args)
	}
	if i := indexOf(args, "--max-turns"); i < 0 || i+1 >= len(args) || args[i+1] != "30" {
		t.Errorf("MAX_TURNS not honoured: %q", args)
	}
	if i := indexOf(args, "--append-system-prompt"); i < 0 || i+1 >= len(args) || !strings.Contains(args[i+1], "<qa>") || strings.Contains(args[i+1], "<verify>") {
		t.Errorf("trailer instruction is not the QA one")
	}

	i := indexOf(args, "--allowedTools")
	if i < 0 {
		t.Fatal("qa mode has no --allowedTools")
	}
	got := args[i+1:]
	want := []string{
		"Read", "Grep", "Glob",
		"mcp__deployment-io__deploy_static_site_preview",
		"mcp__deployment-io__verify_preview_reachable",
		"mcp__deployment-io__verify_preview_rendered",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("allowlist (must be last) = %q, want exactly %q", got, want)
	}
	for _, tool := range got {
		if strings.HasPrefix(tool, "Bash") || tool == "Write" || tool == "Edit" || tool == "NotebookEdit" {
			t.Errorf("qa allowlist carries %q", tool)
		}
	}
}
