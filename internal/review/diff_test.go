package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo builds a real git repository with one commit, and returns its
// start-of-run commit. These tests shell out to git because the diff IS git's
// output: a fake would pin our idea of the format rather than the format.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		run(t, dir, args...)
	}
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "base")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %s", args, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The baseline is the START-OF-RUN commit, not HEAD. An implementer committing
// its own work is supported, and a diff against HEAD would come back empty on
// exactly that path — a review that examined nothing and reported it clean.
func TestComputeDiffsAgainstTheStartCommitEvenWhenTheAgentCommitted(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	write(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() { panic(\"boom\") }\n")
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "the agent's own commit")

	diff := Compute(workDir, map[string]string{"0-acme/api": base})
	if diff.Empty() {
		t.Fatal("diff is empty — the agent's committed work was invisible to the review")
	}
	if !strings.Contains(diff.Text, "panic") {
		t.Errorf("diff does not carry the change:\n%s", diff.Text)
	}
	if !contains(diff.Paths, "0-acme/api/main.go") {
		t.Errorf("paths = %v, want the changed file", diff.Paths)
	}
}

// A new file the agent never staged is the most reviewable change there is.
// `git diff` alone would not mention it.
func TestComputeIncludesUntrackedFiles(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	write(t, filepath.Join(repo, "secrets.go"), "package main\n\nconst token = \"hunter2\"\n")

	diff := Compute(workDir, map[string]string{"0-acme/api": base})
	if !strings.Contains(diff.Text, "hunter2") {
		t.Errorf("untracked file is missing from the diff:\n%s", diff.Text)
	}
	if !contains(diff.Paths, "0-acme/api/secrets.go") {
		t.Errorf("paths = %v, want the untracked file", diff.Paths)
	}
}

func TestComputeCoversEveryRepository(t *testing.T) {
	workDir := t.TempDir()
	api := filepath.Join(workDir, "0-acme", "api")
	web := filepath.Join(workDir, "1-acme", "web")
	apiBase := initRepo(t, api)
	webBase := initRepo(t, web)
	write(t, filepath.Join(api, "main.go"), "package main\n\n// api change\n")
	write(t, filepath.Join(web, "main.go"), "package main\n\n// web change\n")

	diff := Compute(workDir, map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase})
	for _, want := range []string{"api change", "web change", "repository 0-acme/api", "repository 1-acme/web"} {
		if !strings.Contains(diff.Text, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff.Text)
		}
	}
}

// A single generated file must not crowd out every other change, and the
// elision has to be VISIBLE: a silently shortened diff reads as a complete one.
func TestComputeCapsOneFileAndSaysSo(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	var b strings.Builder
	for b.Len() < MaxFileDiffBytes*2 {
		b.WriteString("// a line of generated nonsense that exists only to be long\n")
	}
	write(t, filepath.Join(repo, "generated.go"), "package main\n"+b.String())

	diff := Compute(workDir, map[string]string{"0-acme/api": base})
	if !diff.Truncated {
		t.Error("Truncated = false, want true — the per-file cap fired")
	}
	if !strings.Contains(diff.Text, "elided") {
		t.Errorf("no elision marker in the diff — truncation must be visible:\n%s", firstBytes(diff.Text))
	}
	if len(diff.Text) > MaxDiffBytes+1000 {
		t.Errorf("diff is %d bytes, want it held near the %d cap", len(diff.Text), MaxDiffBytes)
	}
}

func TestComputeOnACleanTreeIsEmpty(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	diff := Compute(workDir, map[string]string{"0-acme/api": base})
	if !diff.Empty() {
		t.Errorf("diff = %+v, want empty for an untouched tree", diff)
	}
}

// A repository that is not a checkout at all must not fail the review: it
// contributes nothing, and the coverage record still describes what ran.
func TestComputeSurvivesAnUnreadableRepository(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "0-acme", "api"), 0o755); err != nil {
		t.Fatal(err)
	}

	diff := Compute(workDir, map[string]string{"0-acme/api": "deadbeef"})
	if !diff.Empty() {
		t.Errorf("diff = %+v, want empty rather than a panic or an error", diff)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func firstBytes(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}
