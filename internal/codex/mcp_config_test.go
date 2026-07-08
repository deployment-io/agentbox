package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// TestBuildArgsMCPConfig verifies the codex half of the C0 transport: with a
// tool socket set, codex gets -c overrides registering the runner's stdio bridge
// as an MCP server; without one, no MCP config is added.
func TestBuildArgsMCPConfig(t *testing.T) {
	d := &Driver{}

	// No socket -> no mcp_servers override.
	for _, a := range d.BuildArgs(&config.Config{StepPrompt: "hi"}) {
		if strings.Contains(a, "mcp_servers") {
			t.Fatalf("did not expect an mcp_servers override without MCPSocket: %q", a)
		}
	}

	// Socket set -> command + args overrides for the deployment_io server.
	const sock = "/run/agentbox/tool-rpc.sock"
	args := d.BuildArgs(&config.Config{StepPrompt: "hi", MCPSocket: sock})

	cmdVal, ok := mcpCOverride(args, "mcp_servers.deployment_io.command")
	if !ok || cmdVal == "" {
		t.Fatalf("missing/empty command override in %v", args)
	}

	argsVal, ok := mcpCOverride(args, "mcp_servers.deployment_io.args")
	if !ok {
		t.Fatalf("missing args override in %v", args)
	}
	var got []string
	if err := json.Unmarshal([]byte(argsVal), &got); err != nil {
		t.Fatalf("args override is not a JSON array: %v (%q)", err, argsVal)
	}
	want := []string{"mcp-bridge", sock}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("bridge args = %v, want %v", got, want)
	}
}

// mcpCOverride returns the RHS of a `-c <key>=<value>` pair, if present.
func mcpCOverride(args []string, key string) (string, bool) {
	prefix := key + "="
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix), true
		}
	}
	return "", false
}
