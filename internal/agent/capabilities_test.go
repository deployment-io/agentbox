package agent_test

import (
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"

	// Side-effect imports register every real Driver, so this test sees
	// the same registry main.go does.
	_ "github.com/deployment-io/agentbox/internal/claude"
	_ "github.com/deployment-io/agentbox/internal/codex"
	_ "github.com/deployment-io/agentbox/internal/opencode"
)

// TestDeclaredCapabilities pins the capability matrix for every registered
// agent. The values are load-bearing beyond this repo: a consumer chooses
// which agent fills which step, and a step whose work is tool invocation
// (deploy a preview, verify it) can only be served by an agent declaring
// MCPTools. Flipping one of these silently would let such a pairing be
// accepted and then fail mid-run.
//
// A new agent shows up here as a compile-time miss (the map lookup fails),
// which is the point: adding a Driver should force a decision about what
// it can do rather than defaulting into a claim.
func TestDeclaredCapabilities(t *testing.T) {
	want := map[string]agent.Capabilities{
		"claude-code": {MCPTools: true},
		"codex":       {MCPTools: true},
		// opencode's driver never points it at the MCP bridge — see the
		// comment on its Capabilities method.
		"opencode": {MCPTools: false},
	}

	for _, agentType := range agent.RegisteredTypes() {
		expected, known := want[agentType]
		if !known {
			t.Errorf("agent %q is registered but has no expected capabilities here; "+
				"add a row declaring what it supports", agentType)
			continue
		}

		d, err := agent.DriverFor(agentType, "")
		if err != nil {
			t.Fatalf("DriverFor(%q): %v", agentType, err)
		}
		if got := d.Capabilities(); got != expected {
			t.Errorf("%s capabilities = %+v, want %+v", agentType, got, expected)
		}
	}
}
