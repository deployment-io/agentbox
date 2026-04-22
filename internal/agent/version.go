package agent

import (
	"os/exec"
	"strings"
)

// DetectVersion runs `claude --version` and returns the trimmed output,
// or "" if the command fails or claude isn't on PATH. Consumers treat
// "" as "unknown".
func DetectVersion() string {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
