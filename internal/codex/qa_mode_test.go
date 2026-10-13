package codex

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func TestBuildArgsInQAMode(t *testing.T) {
	cfg := &config.Config{
		Mode:           config.ModeQA,
		StepPrompt:     "the QA prompt",
		QARepositories: []string{"0-acme/website"},
		MCPSocket:      "/run/agentbox/tool-rpc.sock",
	}
	d := &Driver{}
	args := d.BuildArgs(cfg)
	joined := strings.Join(args, "\n")

	if !strings.Contains(joined, "--sandbox\nread-only") {
		t.Errorf("qa mode is not read-only: %q", args)
	}
	for _, unwanted := range []string{"danger-full-access", "--dangerously-bypass-approvals-and-sandbox"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("qa mode carries %s", unwanted)
		}
	}
	for _, want := range []string{
		"mcp_servers.deployment_io.command=",
		`mcp_servers.deployment_io.args=["mcp-bridge","/run/agentbox/tool-rpc.sock"]`,
		`mcp_servers.deployment_io.default_tools_approval_mode="approve"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("qa mode is missing %s", want)
		}
	}
	if strings.Contains(joined, "the QA prompt") {
		t.Error("the QA prompt must go on stdin, not in the args")
	}
	stdin := d.Stdin(cfg)
	if !strings.HasPrefix(stdin, "the QA prompt\n\n") || !strings.Contains(stdin, "<qa>") || strings.Contains(stdin, "<verify>") {
		t.Errorf("Stdin = %q, want the prompt plus the QA trailer instruction", stdin)
	}
}

// The approval-mode key is QA's alone: the implement run's bypass flag already
// auto-allows the tools.
func TestBuildArgsBatchModeHasNoApprovalMode(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "x", MCPSocket: "/s"})
	if strings.Contains(strings.Join(args, "\n"), "default_tools_approval_mode") {
		t.Error("batch mode sets an MCP approval mode")
	}
}
