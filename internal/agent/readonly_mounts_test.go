package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func reviewWorkDir(t *testing.T) (string, string) {
	t.Helper()
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return workDir, repo
}

// A writable repository withdraws the runner's read-only claim, so the
// reviewer keeps its own sandbox instead of reviewing with write access.
func TestAWritableRepositoryWithdrawsTheReadOnlyClaim(t *testing.T) {
	workDir, repo := reviewWorkDir(t)
	cfg := &config.Config{WorkDir: workDir, ReviewReadOnlyMounts: true}
	var log strings.Builder
	verifyReadOnlyMounts(cfg, &log)
	if cfg.ReviewReadOnlyMounts {
		t.Error("the claim survived a repository that accepted a write")
	}
	if !strings.Contains(log.String(), repo) {
		t.Errorf("the log does not name the writable repository:\n%s", log.String())
	}
	entries, _ := os.ReadDir(repo)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentbox-readonly-probe-") {
			t.Errorf("the probe file %s was left in the repository", e.Name())
		}
	}
}

// Read-only repositories keep the claim.
func TestReadOnlyRepositoriesKeepTheClaim(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so a read-only mount cannot be simulated with chmod")
	}
	workDir, repo := reviewWorkDir(t)
	if err := os.Chmod(repo, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(repo, 0o755) })
	cfg := &config.Config{WorkDir: workDir, ReviewReadOnlyMounts: true}
	verifyReadOnlyMounts(cfg, &strings.Builder{})
	if !cfg.ReviewReadOnlyMounts {
		t.Error("the claim was withdrawn although no repository accepted a write")
	}
}

// Without the claim there is nothing to verify and nothing is written.
func TestNoClaimMeansNoProbe(t *testing.T) {
	workDir, repo := reviewWorkDir(t)
	cfg := &config.Config{WorkDir: workDir}
	var log strings.Builder
	verifyReadOnlyMounts(cfg, &log)
	if cfg.ReviewReadOnlyMounts || log.Len() != 0 {
		t.Errorf("an absent claim was acted on: flag %v, log %q", cfg.ReviewReadOnlyMounts, log.String())
	}
	if entries, _ := os.ReadDir(repo); len(entries) != 1 {
		t.Errorf("the repository gained entries without a claim to verify: %v", entries)
	}
}

// The probed set covers base-commit keys and checkouts found under the work
// dir, and ignores keys that escape it.
func TestReviewRepositoryDirsCoversEveryRepository(t *testing.T) {
	workDir, repo := reviewWorkDir(t)
	cfg := &config.Config{WorkDir: workDir, ReviewBaseCommits: map[string]string{
		"0-acme/api": "abc", "1-acme/web": "def", "../escape": "ghi",
	}}
	got := reviewRepositoryDirs(cfg)
	want := map[string]bool{repo: true, filepath.Join(workDir, "1-acme", "web"): true}
	if len(got) != len(want) {
		t.Fatalf("dirs = %v, want %v", got, want)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected dir %s", d)
		}
	}
}
