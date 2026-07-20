package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeWorkDir builds a /work-like tree: repos live at <owner>/<repo> and are
// identified by a .git entry; context/ and dot-dirs must be ignored.
func makeWorkDir(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	repo := filepath.Join(work, "0-deployment-io", "dashboard")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Non-repo siblings that must NOT be anchored.
	if err := os.MkdirAll(filepath.Join(work, "context"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, ".agentbox-output"), 0o755); err != nil {
		t.Fatal(err)
	}
	return work
}

func TestRepoDirsUnder_FindsReposSkipsNonRepos(t *testing.T) {
	work := makeWorkDir(t)
	got := repoDirsUnder(work)
	want := []string{filepath.Join(work, "0-deployment-io", "dashboard")}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("repoDirsUnder = %v, want %v (context/.agentbox-output must be skipped)", got, want)
	}
}

func TestAnchorPromptToRepos_PrependsRepoPathAndKeepsPrompt(t *testing.T) {
	work := makeWorkDir(t)
	out := anchorPromptToRepos("Add a LICENSE file.", work)
	repo := filepath.Join(work, "0-deployment-io", "dashboard")
	if !strings.Contains(out, repo) {
		t.Errorf("anchored prompt missing repo path %q:\n%s", repo, out)
	}
	if !strings.HasSuffix(out, "Add a LICENSE file.") {
		t.Errorf("original prompt must be preserved at the end:\n%s", out)
	}
	if !strings.Contains(out, "will be discarded") {
		t.Errorf("anchor should warn that out-of-repo writes are discarded:\n%s", out)
	}
}

func TestAnchorPromptToRepos_NoReposIsNoOp(t *testing.T) {
	work := t.TempDir() // no repo subdirs
	prompt := "Analyze the infrastructure and summarize."
	if out := anchorPromptToRepos(prompt, work); out != prompt {
		t.Errorf("with no repos the prompt must be unchanged, got:\n%s", out)
	}
}
