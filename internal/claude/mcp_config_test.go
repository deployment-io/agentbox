package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// TestBuildArgsMCPConfig verifies the agentbox half of the C0 transport: with a
// tool socket set, Claude Code is pointed at the runner via a valid stdio
// bridge server; without one, no MCP flag is added.
func TestBuildArgsMCPConfig(t *testing.T) {
	d := &Driver{}

	// No socket -> no --mcp-config.
	if _, ok := mcpArgValue(d.BuildArgs(&config.Config{StepPrompt: "hi"}), "--mcp-config"); ok {
		t.Fatal("did not expect --mcp-config without MCPSocket")
	}

	// Socket set -> --mcp-config with a valid stdio bridge server.
	const sock = "/run/agentbox/tool-rpc.sock"
	val, ok := mcpArgValue(d.BuildArgs(&config.Config{StepPrompt: "hi", MCPSocket: sock}), "--mcp-config")
	if !ok {
		t.Fatal("expected --mcp-config when MCPSocket is set")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(val), &cfg); err != nil {
		t.Fatalf("mcp-config is not valid JSON: %v (%q)", err, val)
	}
	srv, ok := cfg.MCPServers["deployment-io"]
	if !ok {
		t.Fatalf("missing deployment-io server in %q", val)
	}
	if srv.Command == "" {
		t.Fatal("bridge command is empty")
	}
	want := []string{"mcp-bridge", sock}
	if strings.Join(srv.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("bridge args = %v, want %v", srv.Args, want)
	}
}

func mcpArgValue(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}
