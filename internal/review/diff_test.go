package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deployment-io/agentbox/internal/config"
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

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
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

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
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

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase})
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

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
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

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
	if !diff.Empty() {
		t.Errorf("diff = %+v, want empty for an untouched tree", diff)
	}
}

// A repository that is not a checkout at all FAILS the review. It used to
// contribute nothing and let the round continue, which meant the passes
// reviewed an empty diff and reported it clean — a clean bill of health for
// work nobody looked at, which is the one failure mode a review must not have.
func TestComputeFailsWhenTheRepositoryIsNotACheckout(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "0-acme", "api"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Compute(workDir, map[string]string{"0-acme/api": "deadbeef"})
	if err == nil {
		t.Fatal("Compute returned no error for a directory that is not a git repository")
	}
	if !strings.Contains(err.Error(), "0-acme/api") {
		t.Errorf("error does not name the repository: %v", err)
	}
}

// The same rule for a base commit this clone does not have — a mis-keyed repo
// or a base_commits block that went stale. The diff would be empty and the
// review would pass work it never saw.
func TestComputeFailsWhenTheBaseCommitIsUnknown(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	initRepo(t, repo)
	write(t, filepath.Join(repo, "main.go"), "package main\n\n// a real change\n")

	_, err := Compute(workDir, map[string]string{
		"0-acme/api": "0000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("Compute returned no error for a base commit the clone does not have")
	}
	if !strings.Contains(err.Error(), "000000000000") {
		t.Errorf("error does not name the missing commit: %v", err)
	}
}

// The OVERALL cap, not the per-file one. It exists because the prompt is a
// single argv element and Linux refuses to exec one over 128 KiB: overrunning
// it does not shorten the review, it stops the agent from starting.
func TestComputeCapsTheWholeDiffAcrossFiles(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	// Several files, each comfortably under the per-file cap, together well
	// over the overall one — so only the overall cap can fire.
	var b strings.Builder
	for b.Len() < MaxFileDiffBytes/2 {
		b.WriteString("// a line of generated nonsense that exists only to be long\n")
	}
	body := b.String()
	for i := 0; i < 6; i++ {
		write(t, filepath.Join(repo, fmt.Sprintf("generated%d.go", i)), "package main\n"+body)
	}

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
	if !diff.Truncated {
		t.Error("Truncated = false, want true — the overall cap fired")
	}
	if !strings.Contains(diff.Text, "diff truncated") {
		t.Errorf("no overall elision marker:\n%s", lastBytes(diff.Text))
	}
	if len(diff.Text) > MaxPromptBytes {
		t.Errorf("diff is %d bytes, over the %d prompt cap that keeps execve working",
			len(diff.Text), MaxPromptBytes)
	}
	if !strings.Contains(diff.Text, "generated0.go") {
		t.Error("the first file was dropped; truncation must keep the earliest content")
	}
}

// The whole prompt — diff, spec, briefs — must stay under the argv limit, so
// the review of a very large change still starts.
func TestBuildPromptStaysUnderTheArgvLimit(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	var b strings.Builder
	for b.Len() < MaxFileDiffBytes/2 {
		b.WriteString("// a line of generated nonsense that exists only to be long\n")
	}
	body := b.String()
	for i := 0; i < 8; i++ {
		write(t, filepath.Join(repo, fmt.Sprintf("generated%d.go", i)), "package main\n"+body)
	}

	cfg := &config.Config{
		WorkDir:           workDir,
		ReviewBaseCommits: map[string]string{"0-acme/api": base},
		ReviewPasses:      []string{PassSecurity, PassCorrectness},
		ReviewSpec:        strings.Repeat("a very long spec sentence that goes on. ", 1000),
		ReviewRound:       1,
	}
	plan, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Prompt) > MaxPromptBytes {
		t.Errorf("prompt is %d bytes, over the %d cap", len(plan.Prompt), MaxPromptBytes)
	}
	if !utf8.ValidString(plan.Prompt) {
		t.Error("prompt is not valid UTF-8 — a cap sliced through a rune")
	}
	// Truncating the change and not saying so is the failure this guards.
	for _, c := range plan.Coverage {
		if c.State == stateChecked && c.Reason == "" {
			t.Errorf("parameter %q is checked with no reason, but the diff was truncated", c.Parameter)
		}
	}
}

// A path with non-ASCII characters must diff and cost-gate under its real
// name. Without -z git quotes and octal-escapes it, so "café.md" arrives as
// "\"caf\\303\\251.md\"" — which matches no documentation extension and turns
// a docs-only change into one that spends two model calls.
func TestComputeHandlesNonASCIIPaths(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)

	write(t, filepath.Join(repo, "café.md"), "# documentation\n")

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
	if !contains(diff.Paths, "0-acme/api/café.md") {
		t.Errorf("paths = %q, want the unescaped non-ASCII path", diff.Paths)
	}
	passes, skipped := SelectPasses([]string{PassSecurity, PassCorrectness}, diff)
	if len(passes) != 0 {
		t.Errorf("passes = %v, want none — this is a documentation-only change", passes)
	}
	if skipped[PassSecurity] != ReasonDocsOnly {
		t.Errorf("security skip reason = %q, want %q", skipped[PassSecurity], ReasonDocsOnly)
	}
}

// One header per repository, not one per file. Per-file headers read as though
// each file were its own repo and spend the byte budget on repetition.
func TestComputeEmitsOneHeaderPerRepository(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)
	write(t, filepath.Join(repo, "a.go"), "package main\n\n// a\n")
	write(t, filepath.Join(repo, "b.go"), "package main\n\n// b\n")
	write(t, filepath.Join(repo, "c.go"), "package main\n\n// c\n")

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": base})
	if got := strings.Count(diff.Text, "--- repository 0-acme/api"); got != 1 {
		t.Errorf("repository header appears %d times, want exactly 1:\n%s", got, diff.Text)
	}
}

func mustCompute(t *testing.T, workDir string, baseCommits map[string]string) Diff {
	t.Helper()
	diff, err := Compute(workDir, baseCommits)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return diff
}

func lastBytes(s string) string {
	if len(s) > 500 {
		return "…" + s[len(s)-500:]
	}
	return s
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
