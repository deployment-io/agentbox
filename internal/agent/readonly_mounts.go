package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/deployment-io/agentbox/internal/config"
)

// verifyReadOnlyMounts checks the runner's claim that every repository is
// mounted read-only before any driver acts on it.
//
// REVIEW_READONLY_MOUNTS lets a reviewer drop its own sandbox (Codex's bwrap
// cannot start in these containers). That trade is safe only if the mounts
// really are read-only, and the variable is just a string in the environment:
// a stale runner, a repository added after the mount list was built, or a
// submodule the list missed would leave a tree writable to a reviewer whose
// approvals are bypassed. So each repository is probed with a real write. If
// any write succeeds, the claim is withdrawn — cfg.ReviewReadOnlyMounts is
// cleared, and the driver keeps the agent's own sandbox, which in a
// namespace-less container means a round that fails safely instead of a
// reviewer that can edit the change it is judging.
//
// The probe file is removed immediately and lives only for the probe, before
// the review's diff is computed, so it never appears in the change.
func verifyReadOnlyMounts(cfg *config.Config, log io.Writer) {
	if !cfg.ReviewReadOnlyMounts {
		return
	}
	for _, dir := range reviewRepositoryDirs(cfg) {
		f, err := os.CreateTemp(dir, ".agentbox-readonly-probe-*")
		if err != nil {
			continue // not writable: the claim holds for this repository
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		cfg.ReviewReadOnlyMounts = false
		fmt.Fprintf(log, "[agentbox] review: %s is writable although REVIEW_READONLY_MOUNTS says the repositories are read-only; keeping the agent's own sandbox\n", dir)
		return
	}
}

// reviewRepositoryDirs is every repository directory the review could touch:
// the checkouts found under the work dir (which include a new repository with
// no start commit) plus every base-commit key, deduplicated.
func reviewRepositoryDirs(cfg *config.Config) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		if clean := filepath.Clean(dir); !seen[clean] {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}
	for _, dir := range repoDirsUnder(cfg.WorkDir) {
		add(dir)
	}
	for rel := range cfg.ReviewBaseCommits {
		if filepath.IsLocal(rel) {
			add(filepath.Join(cfg.WorkDir, rel))
		}
	}
	return dirs
}
