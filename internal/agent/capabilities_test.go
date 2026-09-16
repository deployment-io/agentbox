package agent_test

import (
	"sort"
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
// A new agent shows up as an unexpected registry entry when this runs, so
// adding a Driver forces a decision about what it can do rather than
// letting it default into a claim. (The compiler enforces only that the
// method exists — the interface does that; what it declares is on us.)
func TestDeclaredCapabilities(t *testing.T) {
	want := map[string]agent.Capabilities{
		"claude-code": {MCPTools: true},
		"codex":       {MCPTools: true},
		"opencode":    {MCPTools: true},
	}

	registered := agent.RegisteredTypes()

	// Without this the test is vacuous in the one case that matters: if the
	// side-effect imports above are dropped or a driver's init() stops
	// registering, the loop body never runs and this reports PASS while
	// checking nothing — in a test whose entire job is catching that drift.
	if len(registered) != len(want) {
		t.Fatalf("registry has %d agents %v, expected %d %v; a driver was added, "+
			"removed, or failed to register", len(registered), registered, len(want), keys(want))
	}

	for _, agentType := range registered {
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

func keys(m map[string]agent.Capabilities) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
