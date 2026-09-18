package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeRepo creates a committed repository at <workDir>/<rel> and returns its
// directory and HEAD.
func makeRepo(t *testing.T, workDir, rel string) (string, string) {
	t.Helper()
	dir := filepath.Join(workDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, dir, "init", "-q", "-b", "main")
	gitInRepo(t, dir, "config", "user.email", "test@example.com")
	gitInRepo(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-q", "-m", "first")
	return dir, gitInRepo(t, dir, "rev-parse", "HEAD")
}

func TestRecordStartCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	workDir := t.TempDir()
	apiDir, apiSHA := makeRepo(t, workDir, "0-acme/api")
	webDir, webSHA := makeRepo(t, workDir, "1-acme/web")

	// Non-repository neighbours the checkout layout really contains — the
	// runner's dot-prefixed scratch dirs and the context directory. None of
	// them has a commit to record.
	for _, rel := range []string{".agentbox-tmp", ".agentbox-output", "context/notes"} {
		if err := os.MkdirAll(filepath.Join(workDir, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := recordStartCommits(workDir)
	want := map[string]string{apiDir: apiSHA, webDir: webSHA}
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for dir, sha := range want {
		if got[dir] != sha {
			t.Errorf("%s recorded as %q, want %q", dir, got[dir], sha)
		}
	}
}

// The snapshot has to be of the state handed to the run. A later read is a
// different fact: an agent that commits its own work moves HEAD, and a
// baseline taken there would be the agent's own change.
func TestRecordStartCommitsIsASnapshotNotALiveRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	workDir := t.TempDir()
	dir, startSHA := makeRepo(t, workDir, "0-acme/api")

	recorded := recordStartCommits(workDir)

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-q", "-m", "the agent's own commit")

	if recorded[dir] != startSHA {
		t.Errorf("recorded %q, want the pre-agent commit %q", recorded[dir], startSHA)
	}
	if head := gitInRepo(t, dir, "rev-parse", "HEAD"); recorded[dir] == head {
		t.Error("fixture bug: HEAD should have moved")
	}
}

// Best-effort: a repository whose HEAD can't be resolved is simply absent,
// never an error that fails the run. Absence is the failure-closed signal the
// replay already handles.
func TestRecordStartCommitsSkipsUnresolvableRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	workDir := t.TempDir()
	good, goodSHA := makeRepo(t, workDir, "0-acme/api")

	// Unborn HEAD: initialised, never committed.
	unborn := filepath.Join(workDir, "1-acme/fresh")
	if err := os.MkdirAll(unborn, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, unborn, "init", "-q", "-b", "main")

	got := recordStartCommits(workDir)
	if got[good] != goodSHA {
		t.Errorf("the healthy repo was dropped: %v", got)
	}
	if _, ok := got[unborn]; ok {
		t.Errorf("an unborn HEAD was recorded as %q", got[unborn])
	}
}

func TestRecordStartCommitsWithNoRepos(t *testing.T) {
	if got := recordStartCommits(t.TempDir()); got != nil {
		t.Errorf("recordStartCommits = %v, want nil for a repo-less work dir", got)
	}
}
