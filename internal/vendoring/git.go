package vendoring

import (
	"fmt"
	"os/exec"
	"strings"
)

// ConfigureGit installs a global git URL rewrite so module fetches of
// private github.com repos authenticate with token (the GitHub App
// installation token the runner mints for the vendor phase). No-op when
// token is empty — public-only Tasks need no credential. The token is
// written into the ephemeral container's git config only; errors omit
// command output so the token never reaches the logs.
func ConfigureGit(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	key := fmt.Sprintf("url.https://x-access-token:%s@github.com/.insteadOf", token)
	if err := exec.Command("git", "config", "--global", key, "https://github.com/").Run(); err != nil {
		return fmt.Errorf("git config insteadOf failed: %w", err)
	}
	return nil
}
