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
	if !strings.Contains(out, "is discarded") {
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

// The runner writes the organisation's pre-built context for every Task, but
// only the interactive session prompt ever mentioned it — so an implement run
// answered questions about how services are deployed from the code alone,
// beside a directory that already had the answer.
func TestAnchorPromptToRepos_NamesTheContextDirectoryWhenItExists(t *testing.T) {
	work := makeWorkDir(t)
	if err := os.WriteFile(filepath.Join(work, "context", "index.md"), []byte("# services\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := anchorPromptToRepos("Move the API to the new cluster.", work)

	for _, want := range []string{
		filepath.Join(work, "context") + " (start with index.md)",
		"deployment, configuration, or how services connect",
		"a change confined to code does not need it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the anchored prompt does not carry %q:\n%s", want, out)
		}
	}
	// The anchor's own job is unchanged, and the step prompt still comes last.
	if !strings.Contains(out, "is discarded") || !strings.HasSuffix(out, "Move the API to the new cluster.") {
		t.Errorf("the context line displaced the anchor or the prompt:\n%s", out)
	}
}

// And it says nothing when there is nothing to read. An empty context/ is a
// directory the agent would spend a turn opening to learn it is empty, so the
// index file — not the directory — is what the line hangs on.
func TestAnchorPromptToRepos_SilentWithoutAContextIndex(t *testing.T) {
	work := makeWorkDir(t) // makeWorkDir creates context/ but no index.md
	out := anchorPromptToRepos("Rename the handler.", work)
	if strings.Contains(out, "Pre-built context") {
		t.Errorf("an empty context directory was advertised anyway:\n%s", out)
	}
}
