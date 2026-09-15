package agent

import "os"

// bridgeFallbackPath is where the image installs this binary. Used when
// os.Executable can't resolve, which shouldn't happen in the container but
// would otherwise leave the agent with an empty command.
const bridgeFallbackPath = "/usr/local/bin/agentbox"

// BridgeCommand returns the command and args that run this binary as the
// stdio MCP bridge to the runner's tool socket: `agentbox mcp-bridge
// <socket>`, which pipes JSON-RPC to the runner. Tools execute runner-side,
// so credentials never enter the container.
//
// Every agent needs the same bridge invocation but registers it in its own
// shape — claude inlines JSON for --mcp-config, codex sets mcp_servers.*
// via -c, opencode writes an mcp block into its config file. This returns
// the pieces; formatting them stays with each driver.
func BridgeCommand(socket string) (command string, args []string) {
	return selfPath(), []string{"mcp-bridge", socket}
}

func selfPath() string {
	if self, err := os.Executable(); err == nil && self != "" {
		return self
	}
	return bridgeFallbackPath
}
