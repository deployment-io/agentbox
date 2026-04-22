package agent

import (
	"os/exec"
	"strings"
)

// DetectVersion runs `claude --version` and returns the trimmed output.
// Returns an empty string if claude is not on PATH or the command fails
// — agentbox proceeds anyway, and the Outcome's agent_version will be
// empty. Consumers that need the version for reproducibility can detect
// the empty string as "unknown".
//
// Call once per agentbox invocation; there's no benefit to caching, and
// the cost is one fork+exec (~100ms).
func DetectVersion() string {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
