package opencode

import (
	"encoding/json"
	"testing"
)

// The config written here is the only thing that gives opencode a tool
// channel, and a malformed or absent `mcp` block fails as an agent that
// simply never calls a tool — no error, just a run that quietly did less
// than it was asked to. So these assert the document's shape rather than
// that the function returned.

func TestAutonomousConfigWithoutSocketRegistersNoServer(t *testing.T) {
	cfg := agentConfig("", false)

	if _, ok := cfg["mcp"]; ok {
		t.Error("mcp block present with no socket; a plain run would point at a dangling bridge")
	}
	if cfg["permission"] != "allow" {
		t.Errorf("permission = %v, want allow — a headless run must not block on a prompt", cfg["permission"])
	}
}

// Whitespace is worth covering because the value arrives from the
// environment, where config.Load trims it but a direct os.Getenv does not.
func TestAutonomousConfigTreatsBlankSocketAsAbsent(t *testing.T) {
	if _, ok := agentConfig("   ", false)["mcp"]; ok {
		t.Error("whitespace-only socket registered a server")
	}
}

func TestAutonomousConfigRegistersBridgeAsLocalServer(t *testing.T) {
	const socket = "/run/agentbox/tool-rpc.sock"

	raw, err := json.Marshal(agentConfig(socket, false))
	if err != nil {
		t.Fatalf("config is not marshalable: %v", err)
	}

	var got struct {
		Schema string `json:"$schema"`
		MCP    map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("round-trip failed: %v\n%s", err, raw)
	}

	if got.Schema != "https://opencode.ai/config.json" {
		t.Errorf("$schema = %q; opencode validates against it", got.Schema)
	}

	server, ok := got.MCP["deployment-io"]
	if !ok {
		t.Fatalf("no deployment-io server registered; got %v", got.MCP)
	}
	if server.Type != "local" {
		t.Errorf("type = %q, want local — the bridge is stdio, not a URL", server.Type)
	}
	if !server.Enabled {
		t.Error("server registered but disabled")
	}

	// opencode takes command and args as one array, unlike claude (separate
	// args field) and codex (separate -c override).
	if len(server.Command) != 3 {
		t.Fatalf("command = %v, want [<self> mcp-bridge <socket>]", server.Command)
	}
	if server.Command[0] == "" {
		t.Error("command[0] is empty; the bridge binary must resolve")
	}
	if server.Command[1] != "mcp-bridge" {
		t.Errorf("command[1] = %q, want mcp-bridge", server.Command[1])
	}
	if server.Command[2] != socket {
		t.Errorf("command[2] = %q, want the socket path %q", server.Command[2], socket)
	}
}

func TestAutonomousConfigSetsGenerousDiscoveryTimeout(t *testing.T) {
	var got struct {
		MCP map[string]struct {
			Timeout int `json:"timeout"`
		} `json:"mcp"`
	}
	raw, err := json.Marshal(agentConfig("/run/agentbox/tool-rpc.sock", false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}

	// opencode defaults this to 5000ms and, on expiry, loads none of the
	// server's tools rather than erroring — so an under-set value shows up
	// as an agent that silently never calls a tool.
	if got.MCP["deployment-io"].Timeout <= 5000 {
		t.Errorf("discovery timeout = %d, want more than opencode's 5000ms default",
			got.MCP["deployment-io"].Timeout)
	}
}
