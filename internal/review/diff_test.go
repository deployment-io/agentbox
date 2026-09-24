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
	if !strings.Contains(allText(diff), "panic") {
		t.Errorf("diff does not carry the change:\n%s", allText(diff))
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
	if !strings.Contains(allText(diff), "hunter2") {
		t.Errorf("untracked file is missing from the diff:\n%s", allText(diff))
	}
	if !contains(diff.Paths, "0-acme/api/secrets.go") {
		t.Errorf("paths = %v, want the untracked file", diff.Paths)
	}
	if len(diff.Repos) != 1 || !contains(diff.Repos[0].Paths, "secrets.go") {
		t.Errorf("repos = %+v, want the untracked file on the repository's own path list", diff.Repos)
	}
}

func TestComputeCoversEveryRepositoryInSortedOrder(t *testing.T) {
	workDir := t.TempDir()
	api := filepath.Join(workDir, "0-acme", "api")
	web := filepath.Join(workDir, "1-acme", "web")
	apiBase := initRepo(t, api)
	webBase := initRepo(t, web)
	write(t, filepath.Join(api, "main.go"), "package main\n\n// api change\n")
	write(t, filepath.Join(web, "main.go"), "package main\n\n// web change\n")

	diff := mustCompute(t, workDir, map[string]string{"1-acme/web": webBase, "0-acme/api": apiBase})
	if len(diff.Repos) != 2 || diff.Repos[0].Dir != "0-acme/api" || diff.Repos[1].Dir != "1-acme/web" {
		t.Fatalf("repos = %+v, want api then web", diff.Repos)
	}
	if !strings.Contains(diff.Repos[0].Text, "api change") || !strings.Contains(diff.Repos[1].Text, "web change") {
		t.Errorf("each repository must carry its own change: %+v", diff.Repos)
	}
	if diff.Repos[0].Base != apiBase || diff.Repos[1].Base != webBase {
		t.Errorf("bases = %s / %s, want %s / %s", diff.Repos[0].Base, diff.Repos[1].Base, apiBase, webBase)
	}
}

// A repository with no change contributes no entry: the reviewer is not sent
// to read an empty file, and the index does not list a repository as changed.
func TestComputeSkipsAnUntouchedRepository(t *testing.T) {
	workDir := t.TempDir()
	api := filepath.Join(workDir, "0-acme", "api")
	web := filepath.Join(workDir, "1-acme", "web")
	apiBase := initRepo(t, api)
	webBase := initRepo(t, web)
	write(t, filepath.Join(api, "main.go"), "package main\n\n// api change\n")

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase})
	if len(diff.Repos) != 1 || diff.Repos[0].Dir != "0-acme/api" {
		t.Errorf("repos = %+v, want only the changed repository", diff.Repos)
	}
}

// NOTHING IS CAPPED. The diff used to be folded into the prompt under a byte
// budget that dropped later files and later repositories on the floor; now it
// goes to a file, and a file has no budget. A change several times the old cap
// arrives whole, every file and every repository.
func TestComputeNeverTruncates(t *testing.T) {
	workDir := t.TempDir()
	a := filepath.Join(workDir, "0-acme", "a")
	c := filepath.Join(workDir, "1-acme", "c")
	aBase := initRepo(t, a)
	cBase := initRepo(t, c)

	var b strings.Builder
	for b.Len() < 60000 {
		b.WriteString("// a line of generated nonsense that exists only to be long\n")
	}
	for i := 0; i < 8; i++ {
		write(t, filepath.Join(a, fmt.Sprintf("generated%d.go", i)), "package a\n"+b.String())
	}
	write(t, filepath.Join(c, "late.go"), "package c\n// tiny change that used to fall off the page\n")

	diff := mustCompute(t, workDir, map[string]string{"0-acme/a": aBase, "1-acme/c": cBase})
	text := allText(diff)
	if len(text) < 8*60000 {
		t.Errorf("diff is %d bytes, want every one of the eight large files in full", len(text))
	}
	for i := 0; i < 8; i++ {
		if !strings.Contains(text, fmt.Sprintf("b/generated%d.go", i)) {
			t.Errorf("generated%d.go is missing from the diff", i)
		}
	}
	if !strings.Contains(text, "used to fall off the page") {
		t.Error("the second repository's change is missing")
	}
	if strings.Contains(text, "elided") || strings.Contains(text, "truncated") {
		t.Error("the diff carries an elision marker; nothing should be cut")
	}
	if !contains(diff.Paths, "1-acme/c/late.go") {
		t.Errorf("paths = %v, want the second repository's file", diff.Paths)
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

// Write puts one file per repository under <workDir>/.review, BESIDE the
// checkouts and never inside one, and replaces whatever a previous round left
// there. The file is byte-for-byte the repository's diff.
func TestWriteWritesOneFilePerRepositoryAndReplacesTheLastRound(t *testing.T) {
	workDir := t.TempDir()
	api := filepath.Join(workDir, "0-acme", "api")
	web := filepath.Join(workDir, "1-acme", "web")
	apiBase := initRepo(t, api)
	webBase := initRepo(t, web)
	write(t, filepath.Join(api, "main.go"), "package main\n\n// api change\n")
	write(t, filepath.Join(web, "main.go"), "package main\n\n// web change\n")
	stale := filepath.Join(workDir, DirName, "2-acme", "gone.diff")
	write(t, stale, "a diff from a round that was killed before it cleaned up")

	diff := mustCompute(t, workDir, map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase})
	if err := Write(workDir, &diff); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, r := range diff.Repos {
		want := filepath.Join(workDir, DirName, r.Dir+".diff")
		if r.File != want {
			t.Errorf("repo %s File = %s, want %s", r.Dir, r.File, want)
		}
		got, err := os.ReadFile(r.File)
		if err != nil {
			t.Fatalf("diff file for %s was not written: %v", r.Dir, err)
		}
		if string(got) != r.Text {
			t.Errorf("diff file for %s differs from the computed diff", r.Dir)
		}
		if strings.HasPrefix(r.File, api+string(filepath.Separator)) || strings.HasPrefix(r.File, web+string(filepath.Separator)) {
			t.Errorf("diff file %s is inside a repository checkout", r.File)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale diff file from a previous round survived Write")
	}
	// The written files must not themselves become part of the change.
	again := mustCompute(t, workDir, map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase})
	if len(again.Paths) != len(diff.Paths) {
		t.Errorf("paths after Write = %v, want unchanged %v", again.Paths, diff.Paths)
	}
}

func TestCleanupRemovesTheDiffDirectoryAndTolerateItsAbsence(t *testing.T) {
	workDir := t.TempDir()
	if err := Cleanup(workDir); err != nil {
		t.Fatalf("Cleanup on a work dir with no diff directory: %v", err)
	}
	write(t, filepath.Join(workDir, DirName, "0-acme", "api.diff"), "x")
	if err := Cleanup(workDir); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, DirName)); !os.IsNotExist(err) {
		t.Error("the diff directory survived Cleanup")
	}
}

// Build writes the diff files and the prompt names every one of them, with
// the instruction to read them in full before the first pass. The prompt
// carries the index, never the diff: its size does not grow with the change.
func TestBuildNamesEveryDiffFileAndKeepsTheDiffOutOfThePrompt(t *testing.T) {
	workDir := t.TempDir()
	api := filepath.Join(workDir, "0-acme", "api")
	web := filepath.Join(workDir, "1-acme", "web")
	apiBase := initRepo(t, api)
	webBase := initRepo(t, web)
	var big strings.Builder
	for big.Len() < 200000 {
		big.WriteString("// a line of generated nonsense that exists only to be long\n")
	}
	write(t, filepath.Join(api, "generated.go"), "package api\n"+big.String())
	write(t, filepath.Join(web, "handler.go"), "package web\n// sentinel-change-text\n")

	plan, err := Build(&config.Config{
		WorkDir:           workDir,
		ReviewBaseCommits: map[string]string{"0-acme/api": apiBase, "1-acme/web": webBase},
		ReviewPasses:      []string{PassSecurity, PassCorrectness},
		ReviewRound:       1,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, r := range plan.Diff.Repos {
		if r.File == "" {
			t.Fatalf("repo %s has no diff file after Build", r.Dir)
		}
		if _, err := os.Stat(r.File); err != nil {
			t.Errorf("diff file %s missing: %v", r.File, err)
		}
		if !strings.Contains(plan.Prompt, r.File) {
			t.Errorf("prompt does not name the diff file %s:\n%s", r.File, firstBytes(plan.Prompt))
		}
	}
	if !strings.Contains(plan.Prompt, "READ EVERY DIFF FILE IN FULL") {
		t.Error("prompt does not insist the diff files are read before the passes")
	}
	if strings.Contains(plan.Prompt, "sentinel-change-text") {
		t.Error("the diff text itself is in the prompt; only the index should be")
	}
	if len(plan.Prompt) > 8000 {
		t.Errorf("prompt is %d bytes for a two-file change; it must not scale with the diff", len(plan.Prompt))
	}
	if !strings.Contains(plan.Prompt, "handler.go") || !strings.Contains(plan.Prompt, "generated.go") {
		t.Error("the index does not list the changed paths")
	}
	for _, c := range plan.Coverage {
		if c.State == stateChecked && c.Reason != "" {
			t.Errorf("parameter %q checked with reason %q; nothing was truncated", c.Parameter, c.Reason)
		}
	}
}

// The index caps the paths it lists per repository and says how many more
// there are; the diff file still carries every one.
func TestBuildPromptCapsThePathIndexPerRepository(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)
	for i := 0; i < MaxIndexPathsPerRepo+25; i++ {
		write(t, filepath.Join(repo, fmt.Sprintf("f%03d.go", i)), "package api\n")
	}
	plan, err := Build(&config.Config{
		WorkDir:           workDir,
		ReviewBaseCommits: map[string]string{"0-acme/api": base},
		ReviewPasses:      []string{PassSecurity, PassCorrectness},
		ReviewRound:       1,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(plan.Prompt, "(and 25 more") {
		t.Errorf("prompt does not say how many paths the index left out:\n%s", plan.Prompt[len(plan.Prompt)-1500:])
	}
	if strings.Contains(plan.Prompt, fmt.Sprintf("f%03d.go", MaxIndexPathsPerRepo+10)) {
		t.Error("the index lists a path past its cap")
	}
	if len(plan.Diff.Repos[0].Paths) != MaxIndexPathsPerRepo+25 {
		t.Errorf("the repository's own path list was capped: %d", len(plan.Diff.Repos[0].Paths))
	}
}

// Build writes nothing when the cost gate stands every pass down: a docs-only
// change needs no reviewer and must not leave files for one.
func TestBuildWritesNoFilesWhenNothingWillRun(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)
	write(t, filepath.Join(repo, "README.md"), "# docs only\n")
	plan, err := Build(&config.Config{
		WorkDir:           workDir,
		ReviewBaseCommits: map[string]string{"0-acme/api": base},
		ReviewPasses:      []string{PassSecurity, PassCorrectness},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !plan.NothingToReview() {
		t.Fatalf("passes = %v, want none for a docs-only change", plan.Passes)
	}
	if _, err := os.Stat(filepath.Join(workDir, DirName)); !os.IsNotExist(err) {
		t.Error("diff files were written for a round that runs no pass")
	}
}

// A huge spec is cut on a rune boundary; the prompt stays valid UTF-8 and the
// change index still follows it.
func TestBuildPromptCapsTheSpecWithoutBreakingUTF8(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)
	write(t, filepath.Join(repo, "main.go"), "package main\n// changed\n")
	plan, err := Build(&config.Config{
		WorkDir:           workDir,
		ReviewBaseCommits: map[string]string{"0-acme/api": base},
		ReviewPasses:      []string{PassSecurity, PassCorrectness},
		ReviewSpec:        strings.Repeat("café spec sentence that goes on. ", 1000),
		ReviewRound:       1,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !utf8.ValidString(plan.Prompt) {
		t.Error("prompt is not valid UTF-8 — the spec cap sliced through a rune")
	}
	if !strings.Contains(plan.Prompt, "spec truncated") {
		t.Error("the spec cap fired silently")
	}
	if !strings.Contains(plan.Prompt, "[The change under review]") {
		t.Error("the change index is missing after a capped spec")
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

func mustCompute(t *testing.T, workDir string, baseCommits map[string]string) Diff {
	t.Helper()
	diff, err := Compute(workDir, baseCommits)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return diff
}

// allText joins every repository's diff, for assertions about the change as
// a whole.
func allText(d Diff) string {
	var b strings.Builder
	for _, r := range d.Repos {
		b.WriteString(r.Text)
	}
	return b.String()
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

// config.knownReviewPasses and review.parameterForPass are a hand-kept
// mirror (config cannot import review). This is the test the config comment
// promises: the two lists name exactly the same passes.
func TestConfigAndReviewAgreeOnThePassList(t *testing.T) {
	known := config.KnownReviewPasses()
	if len(known) != len(parameterForPass) {
		t.Errorf("config knows %d passes, review maps %d: %v vs %v", len(known), len(parameterForPass), known, parameterForPass)
	}
	for _, p := range known {
		if _, ok := parameterForPass[p]; !ok {
			t.Errorf("config accepts pass %q but review maps it to no parameter", p)
		}
	}
	for p := range parameterForPass {
		if !contains(known, p) {
			t.Errorf("review maps pass %q but config would drop it as unknown", p)
		}
	}
}

// The review prompt states the turn budget when there is one, so a reviewer
// nearing it reports instead of being cut off with nothing; and says nothing
// when there is no cap to plan against.
func TestBuildPromptStatesTheTurnBudget(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initRepo(t, repo)
	write(t, filepath.Join(repo, "main.go"), "package main\n// changed\n")
	build := func(maxTurns string) string {
		plan, err := Build(&config.Config{
			WorkDir:           workDir,
			ReviewBaseCommits: map[string]string{"0-acme/api": base},
			ReviewPasses:      []string{PassSecurity, PassCorrectness},
			ReviewRound:       1,
			MaxTurns:          maxTurns,
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return plan.Prompt
	}
	with := build("80")
	if !strings.Contains(with, "at most 80 turns") || !strings.Contains(with, "write your report") {
		t.Errorf("a prompt with a cap does not state the budget:\n%s", with)
	}
	if strings.Index(with, "[Turn budget]") > strings.Index(with, "[Passes]") {
		t.Error("the turn budget comes after the passes; it must be read before the work starts")
	}
	for _, none := range []string{"", "0", "abc"} {
		if p := build(none); strings.Contains(p, "[Turn budget]") {
			t.Errorf("MAX_TURNS %q produced a budget section", none)
		}
	}
}
