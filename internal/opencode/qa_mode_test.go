package opencode

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func TestAgentConfigInQAMode(t *testing.T) {
	cfg := agentConfig("/run/agentbox/tool-rpc.sock", config.ModeQA)
	perm, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("permission = %#v, want the review permission set", cfg["permission"])
	}
	for _, class := range []string{"edit", "bash", "webfetch"} {
		if perm[class] != "deny" {
			t.Errorf("permission.%s = %v, want deny", class, perm[class])
		}
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatal("qa mode has no mcp block")
	}
	server, ok := mcp["deployment-io"].(map[string]any)
	if !ok || server["type"] != "local" || server["enabled"] != true {
		t.Fatalf("mcp.deployment-io = %#v", mcp["deployment-io"])
	}
	command, _ := server["command"].([]string)
	if strings.Join(command[1:], " ") != "mcp-bridge /run/agentbox/tool-rpc.sock" {
		t.Errorf("command = %q", command)
	}
}

func TestBuildArgsInQAMode(t *testing.T) {
	cfg := &config.Config{Mode: config.ModeQA, StepPrompt: "the QA prompt"}
	d := &Driver{}
	if joined := strings.Join(d.BuildArgs(cfg), "\n"); strings.Contains(joined, "the QA prompt") {
		t.Error("the QA prompt must go on stdin, not in the args")
	}
	stdin := d.Stdin(cfg)
	if !strings.HasPrefix(stdin, "the QA prompt\n\n") || !strings.Contains(stdin, "<qa>") || strings.Contains(stdin, "<verify>") {
		t.Errorf("Stdin = %q, want the prompt plus the QA trailer instruction", stdin)
	}
}
