package verify

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployment-io/agentbox/internal/result"
)

// repoFixture is one repository inside a fake work dir, in the runner's
// /work/<idx>-<owner>/<repo> layout.
type repoFixture struct {
	t       *testing.T
	workDir string
	rel     string // e.g. "0-acme/api"
	dir     string
}

// newRepo creates a git repository with check.sh at the given exit code and
// returns the fixture plus the commit that state is at — the value the
// orchestrator would have recorded before the agent started.
func newRepo(t *testing.T, workDir, rel string, baselineExit int) (*repoFixture, string) {
	t.Helper()
	dir := filepath.Join(workDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &repoFixture{t: t, workDir: workDir, rel: rel, dir: dir}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test")
	r.writeCheck(baselineExit)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	return r, r.head()
}

func (r *repoFixture) git(args ...string) string {
	r.t.Helper()
	full := append([]string{"-C", r.dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repoFixture) head() string { return r.git("rev-parse", "HEAD") }

// writeCheck installs a check.sh that exits with the given code, printing a
// code-specific marker on stderr so a replay's provenance is identifiable.
func (r *repoFixture) writeCheck(exit int) {
	r.t.Helper()
	body := "#!/bin/sh\necho \"check exit=" + itoa(exit) + "\" >&2\nexit " + itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(r.dir, "check.sh"), []byte(body), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}

const checkCmd = "sh ./check.sh"

// failedStep builds the step an agent would report for a repo whose verify
// failed.
func failedStep(rel string) result.VerifyStep {
	return result.VerifyStep{Repo: rel, Command: checkCmd, Passed: false, StderrTail: "agent saw it fail"}
}

func failedRollup(steps ...result.VerifyStep) *result.VerifyResult {
	return &result.VerifyResult{Ran: true, Passed: false, Command: checkCmd, Steps: steps}
}

func newWorkDir(t *testing.T) string {
	t.Helper()
	// Symlink-free: macOS /tmp is a symlink and the work-dir containment
	// check compares cleaned paths.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// THE REGRESSION THIS PACKAGE EXISTS FOR.
//
// An agent that commits its own work (explicitly supported by the runner:
// clean worktree, ahead of origin → pushed as-is) leaves HEAD pointing at
// ITS OWN change. Replaying on HEAD would run the agent's broken code, watch
// it fail, and conclude the failure was pre-existing — pushing genuinely
// broken code. Only the commit recorded BEFORE the agent started gives the
// right answer.
func TestBaselineUsesRecordedStartCommitNotHEAD(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 0)

	// The agent introduces a genuinely new failure AND commits it.
	repo.writeCheck(1)
	repo.git("add", "-A")
	repo.git("commit", "-q", "-m", "agent's own commit")
	if repo.head() == startSHA {
		t.Fatal("fixture bug: HEAD should have moved past the recorded commit")
	}

	vr := failedRollup(failedStep("0-acme/api"))
	var log bytes.Buffer
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
		Log:          &log,
	})

	step := vr.Steps[0]
	if !step.BaselineRan {
		t.Fatalf("baseline did not run: log=%s", log.String())
	}
	if !step.BaselinePassed {
		t.Error("baseline_passed = false — the replay used HEAD (the agent's own commit), " +
			"not the recorded start-of-run commit")
	}
	if vr.PreExisting {
		t.Error("pre_existing = true for a failure the agent introduced — the runner would push broken code")
	}
	if !strings.Contains(log.String(), "replaying on base commit "+startSHA) {
		t.Errorf("log must name the recorded start commit, got: %s", log.String())
	}
	if !strings.Contains(log.String(), "passes on base; failure is new") {
		t.Errorf("log must say the failure is new, got: %s", log.String())
	}
}

// The other half: a failure that was already there before the agent ran.
func TestPreExistingWhenBaselineAlsoFails(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 1)

	// The agent edited something unrelated and left the failure in place.
	if err := os.WriteFile(filepath.Join(repo.dir, "README.md"), []byte("agent was here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	vr := failedRollup(failedStep("0-acme/api"))
	var log bytes.Buffer
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
		Log:          &log,
	})

	step := vr.Steps[0]
	if !step.BaselineRan || step.BaselinePassed {
		t.Fatalf("want baseline_ran && !baseline_passed, got %+v (log=%s)", step, log.String())
	}
	if step.BaselineStderrTail == "" {
		t.Error("a failing baseline must carry its output — the PR body and job log quote it")
	}
	if !strings.Contains(step.BaselineStderrTail, "check exit=1") {
		t.Errorf("baseline tail = %q, want the baseline command's own output", step.BaselineStderrTail)
	}
	if !vr.PreExisting {
		t.Error("pre_existing = false although the only failed step also fails on base")
	}
	if !strings.Contains(log.String(), "fails on base too (pre-existing)") {
		t.Errorf("log must name the verdict, got: %s", log.String())
	}
}

// pre_existing is an ALL, not an ANY: one genuinely new failure anywhere in
// the run must keep the gate closed even when every other failure is old.
func TestPreExistingRequiresEveryFailedStep(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	old, oldSHA := newRepo(t, workDir, "0-acme/api", 1) // failing before the agent
	fresh, freshSHA := newRepo(t, workDir, "1-acme/web", 0)
	fresh.writeCheck(1) // the agent broke this one

	vr := failedRollup(failedStep("0-acme/api"), failedStep("1-acme/web"))
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{old.dir: oldSHA, fresh.dir: freshSHA},
	})

	if !vr.Steps[0].BaselineRan || vr.Steps[0].BaselinePassed {
		t.Errorf("api step: want a failing baseline, got %+v", vr.Steps[0])
	}
	if !vr.Steps[1].BaselineRan || !vr.Steps[1].BaselinePassed {
		t.Errorf("web step: want a passing baseline, got %+v", vr.Steps[1])
	}
	if vr.PreExisting {
		t.Error("pre_existing = true although one failure is new")
	}
}

// Passing steps are left entirely alone — nothing to replay, and the
// baseline fields must not imply a comparison that never happened.
func TestPassingStepsAreNotReplayed(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 1)
	okRepo, okSHA := newRepo(t, workDir, "1-acme/web", 0)

	vr := failedRollup(
		failedStep("0-acme/api"),
		result.VerifyStep{Repo: "1-acme/web", Command: checkCmd, Passed: true},
	)
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA, okRepo.dir: okSHA},
	})

	if vr.Steps[1].BaselineRan {
		t.Error("a passing step was replayed")
	}
	if !vr.PreExisting {
		t.Error("the only FAILED step is pre-existing, so the rollup is too")
	}
}

// FAILURE-CLOSED. Every way a baseline can be unavailable must produce
// baseline_ran:false and must NOT set pre_existing — "we could not tell" and
// "the failure is new" have to lead to the same place.
func TestFailureClosedOnMissingBaseline(t *testing.T) {
	mustGit(t)

	cases := []struct {
		name string
		// setup returns the step repo field and the start-commit map.
		setup func(t *testing.T, workDir string) (string, map[string]string)
	}{
		{
			name: "repo has no recorded start commit",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				newRepo(t, workDir, "0-acme/api", 1)
				return "0-acme/api", map[string]string{}
			},
		},
		{
			name: "path traversal out of the work dir",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				repo, sha := newRepo(t, workDir, "0-acme/api", 1)
				return "../../etc", map[string]string{repo.dir: sha}
			},
		},
		{
			name: "absolute path",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				repo, sha := newRepo(t, workDir, "0-acme/api", 1)
				return repo.dir, map[string]string{repo.dir: sha}
			},
		},
		{
			name: "empty repo field",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				repo, sha := newRepo(t, workDir, "0-acme/api", 1)
				return "", map[string]string{repo.dir: sha}
			},
		},
		{
			name: "recorded commit is no longer reachable",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				repo, _ := newRepo(t, workDir, "0-acme/api", 1)
				return "0-acme/api", map[string]string{repo.dir: "0123456789012345678901234567890123456789"}
			},
		},
		{
			name: "directory appeared mid-run",
			setup: func(t *testing.T, workDir string) (string, map[string]string) {
				repo, sha := newRepo(t, workDir, "0-acme/api", 1)
				return "2-acme/new", map[string]string{repo.dir: sha}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workDir := newWorkDir(t)
			repoField, commits := tc.setup(t, workDir)

			vr := failedRollup(failedStep(repoField))
			var log bytes.Buffer
			Annotate(context.Background(), vr, Options{WorkDir: workDir, StartCommits: commits, Log: &log})

			if vr.Steps[0].BaselineRan {
				t.Error("baseline_ran = true although no baseline could be established")
			}
			if vr.PreExisting {
				t.Error("pre_existing = true with no baseline — the gate must stay closed")
			}
			if !strings.Contains(log.String(), "treating failure as new") {
				t.Errorf("log must say the failure is being treated as new, got: %s", log.String())
			}
		})
	}
}

// A step with no command is as unreplayable as one with no baseline.
func TestFailureClosedOnMissingCommand(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, sha := newRepo(t, workDir, "0-acme/api", 1)

	vr := failedRollup(result.VerifyStep{Repo: "0-acme/api", Passed: false})
	Annotate(context.Background(), vr, Options{WorkDir: workDir, StartCommits: map[string]string{repo.dir: sha}})

	if vr.Steps[0].BaselineRan || vr.PreExisting {
		t.Errorf("want baseline_ran=false and pre_existing=false, got %+v pre_existing=%v", vr.Steps[0], vr.PreExisting)
	}
}

// The agent's checkout is what the runner is about to commit from. A replay
// that touched it — via stash, checkout, or a leftover worktree registration
// — would corrupt the Step's output.
func TestAgentWorktreeIsUntouched(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 0)

	// Uncommitted agent work, exactly as CommitAndPush would find it.
	repo.writeCheck(1)
	if err := os.WriteFile(filepath.Join(repo.dir, "new.txt"), []byte("agent output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, repo.dir)
	statusBefore := repo.git("status", "--porcelain")

	vr := failedRollup(failedStep("0-acme/api"))
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
	})

	if got := snapshotTree(t, repo.dir); got != before {
		t.Errorf("working tree changed across replay:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if got := repo.git("status", "--porcelain"); got != statusBefore {
		t.Errorf("git status changed across replay: %q → %q", statusBefore, got)
	}
	if got := repo.head(); got != startSHA {
		t.Errorf("HEAD moved: %s → %s", startSHA, got)
	}
	if list := repo.git("worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Errorf("a worktree was left registered:\n%s", list)
	}
	entries, err := os.ReadDir(filepath.Join(workDir, tmpDirRel))
	if err == nil && len(entries) != 0 {
		t.Errorf("scratch dirs left behind: %v", entries)
	}
}

// snapshotTree renders every tracked path's name, mode and content under dir,
// excluding .git, so "byte-identical" is checkable in one comparison.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sb.WriteString(rel + " " + info.Mode().String() + " " + string(data) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// A replay that outlives its per-step cap is not a verdict. Reporting one
// would mean a slow test suite silently reads as "pre-existing".
func TestPerStepTimeoutReportsNoBaseline(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, sha := newRepo(t, workDir, "0-acme/api", 1)

	vr := failedRollup(result.VerifyStep{Repo: "0-acme/api", Command: "sleep 30", Passed: false})
	var log bytes.Buffer
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: sha},
		StepTimeout:  100 * time.Millisecond,
		Log:          &log,
	})

	if vr.Steps[0].BaselineRan || vr.PreExisting {
		t.Errorf("a timed-out replay must not be a verdict: %+v pre_existing=%v", vr.Steps[0], vr.PreExisting)
	}
}

// The whole-run budget, not just the per-step cap: N failed steps must not
// be able to serially approach the runner's container wall-clock cap.
func TestTotalBudgetStopsFurtherReplays(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, sha := newRepo(t, workDir, "0-acme/api", 1)

	vr := failedRollup(failedStep("0-acme/api"), failedStep("0-acme/api"))
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: sha},
		TotalBudget:  time.Nanosecond,
	})

	for i, s := range vr.Steps {
		if s.BaselineRan {
			t.Errorf("step %d ran despite an exhausted budget", i)
		}
	}
	if vr.PreExisting {
		t.Error("pre_existing = true with no baselines established")
	}
}

// A SIGTERM during replay must abort it, not extend shutdown by the step
// timeout.
func TestCancelledContextAbortsReplay(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, sha := newRepo(t, workDir, "0-acme/api", 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	vr := failedRollup(result.VerifyStep{Repo: "0-acme/api", Command: "sleep 30", Passed: false})
	Annotate(ctx, vr, Options{WorkDir: workDir, StartCommits: map[string]string{repo.dir: sha}})

	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("replay took %s on a cancelled context", elapsed)
	}
	if vr.Steps[0].BaselineRan || vr.PreExisting {
		t.Errorf("a cancelled replay must not be a verdict: %+v", vr.Steps[0])
	}
}

// Everything that is not "the agent ran a verify that failed per-step" is a
// no-op — including the legacy payload shape, which must round-trip with
// pre_existing absent so an older runner's behaviour is unchanged.
func TestAnnotateNoOps(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)

	cases := map[string]*result.VerifyResult{
		"nil":              nil,
		"skipped":          {Ran: false, SkippedReason: "docs-only"},
		"passed":           {Ran: true, Passed: true, Command: checkCmd},
		"legacy, no steps": {Ran: true, Passed: false, Command: checkCmd, StderrTail: "boom"},
		"all steps reported passing": {Ran: true, Passed: false, Steps: []result.VerifyStep{
			{Repo: "0-acme/api", Command: checkCmd, Passed: true},
		}},
	}
	for name, vr := range cases {
		t.Run(name, func(t *testing.T) {
			Annotate(context.Background(), vr, Options{WorkDir: workDir})
			if vr != nil && vr.PreExisting {
				t.Error("pre_existing must stay false")
			}
		})
	}
}

// The replay must not inherit a parent go.work: the baseline worktree sits
// outside any workspace, and an inherited one would resolve the module's
// packages back to the AGENT'S checkout — replaying the agent's code and
// calling it the baseline.
func TestReplayEnvForcesGoworkOff(t *testing.T) {
	t.Setenv("GOWORK", "/somewhere/go.work")
	t.Setenv("GOMODCACHE", "/cache/gomod")

	env := replayEnv()
	var gowork []string
	var sawCache bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOWORK=") {
			gowork = append(gowork, kv)
		}
		if kv == "GOMODCACHE=/cache/gomod" {
			sawCache = true
		}
	}
	if len(gowork) != 1 || gowork[0] != "GOWORK=off" {
		t.Errorf("GOWORK entries = %v, want exactly [GOWORK=off]", gowork)
	}
	if !sawCache {
		t.Error("the agent's shared caches must be inherited, or the replay refetches everything")
	}
}

// The replay runs in the baseline worktree, and it must see the agent's
// installed dependencies rather than an empty tree — reinstalling needs
// network the agent container doesn't have.
func TestDepDirsAreLinkedIntoTheWorktree(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, sha := newRepo(t, workDir, "0-acme/api", 0)
	if err := os.MkdirAll(filepath.Join(repo.dir, "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.dir, "node_modules", "left-pad", "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The command asserts the dependency is visible from the worktree's cwd.
	vr := failedRollup(result.VerifyStep{
		Repo: "0-acme/api", Command: "test -f node_modules/left-pad/index.js", Passed: false,
	})
	Annotate(context.Background(), vr, Options{WorkDir: workDir, StartCommits: map[string]string{repo.dir: sha}})

	if !vr.Steps[0].BaselineRan {
		t.Fatal("baseline did not run")
	}
	if !vr.Steps[0].BaselinePassed {
		t.Error("node_modules was not visible in the baseline worktree")
	}
}

func TestResolveRepoDir(t *testing.T) {
	cases := []struct {
		repo    string
		want    string
		wantErr bool
	}{
		{repo: "0-acme/api", want: "/work/0-acme/api"},
		{repo: "  0-acme/api  ", want: "/work/0-acme/api"},
		{repo: "0-acme/../0-acme/api", want: "/work/0-acme/api"},
		{repo: "", wantErr: true},
		{repo: ".", wantErr: true},
		{repo: "..", wantErr: true},
		{repo: "../outside", wantErr: true},
		{repo: "0-acme/../../outside", wantErr: true},
		{repo: "/etc/passwd", wantErr: true},
		{repo: "/work/0-acme/api", wantErr: true},
	}
	for _, tc := range cases {
		got, err := resolveRepoDir("/work", tc.repo)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveRepoDir(%q) = %q, want an error", tc.repo, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("resolveRepoDir(%q) = (%q, %v), want %q", tc.repo, got, err, tc.want)
		}
	}
}

func TestPickTailKeepsTheEnd(t *testing.T) {
	noise := strings.Repeat("compiling\n", 1000)
	got := pickTail(noise+"FAIL: the actual reason", "ignored stdout")
	if !strings.Contains(got, "FAIL: the actual reason") {
		t.Error("truncation dropped the end, which is where the failure is")
	}
	if !strings.HasPrefix(got, "…") {
		t.Error("a truncated tail should say so")
	}
	if len(got) > tailMaxBytes+len("…") {
		t.Errorf("tail is %d bytes, want it bounded near %d", len(got), tailMaxBytes)
	}
	if got := pickTail("   ", "stdout instead"); got != "stdout instead" {
		t.Errorf("stdout fallback = %q", got)
	}
}

// The <verify> payload is JSON the AGENT wrote, so it can already contain
// the fields this package owns. An agent that emits "pre_existing":true —
// by mistake, by copying the schema out of the docs, or deliberately — would
// otherwise walk its own broken build straight past the runner's gate.
func TestAgentAssertedBaselineClaimsAreDiscarded(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 0)
	repo.writeCheck(1) // the agent broke it

	vr := &result.VerifyResult{
		Ran: true, Passed: false, Command: checkCmd,
		PreExisting: true, // the agent's claim
		Steps: []result.VerifyStep{{
			Repo: "0-acme/api", Command: checkCmd, Passed: false,
			BaselineRan: true, BaselinePassed: false,
			BaselineStderrTail: "the agent made this up",
		}},
	}
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
	})

	if vr.PreExisting {
		t.Error("the agent's own pre_existing claim survived — the gate is bypassable from the prompt")
	}
	if !vr.Steps[0].BaselinePassed {
		t.Error("the replay's verdict must replace the agent's, and the base commit passes")
	}
	if vr.Steps[0].BaselineStderrTail != "" {
		t.Errorf("agent-authored baseline tail survived: %q", vr.Steps[0].BaselineStderrTail)
	}
}

// Same for the paths Annotate returns early on: a skipped or passing verify
// carries no baseline, so any claim of one must be dropped rather than
// passed through to result.json.
func TestAgentAssertedClaimsAreDiscardedOnEarlyReturns(t *testing.T) {
	for name, vr := range map[string]*result.VerifyResult{
		"skipped": {Ran: false, SkippedReason: "docs-only", PreExisting: true},
		"passed":  {Ran: true, Passed: true, Command: checkCmd, PreExisting: true},
		"no failed steps": {Ran: true, Passed: false, PreExisting: true, Steps: []result.VerifyStep{
			{Repo: "0-acme/api", Command: checkCmd, Passed: true, BaselineRan: true},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			Annotate(context.Background(), vr, Options{WorkDir: t.TempDir()})
			if vr.PreExisting {
				t.Error("pre_existing survived an early return")
			}
			for i, s := range vr.Steps {
				if s.BaselineRan {
					t.Errorf("step %d kept an agent-asserted baseline_ran", i)
				}
			}
		})
	}
}

// A single-repo run reports the legacy rollup with no steps — the prompt only
// asks for steps with more than one repository. That rollup is the one
// repository's step and must get a baseline like any other, or the common
// case would be the one case the gate can never open for.
func TestSingleRepoRollupIsReplayedAsItsOwnStep(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 1) // red before the agent

	vr := &result.VerifyResult{Ran: true, Passed: false, Command: checkCmd, StderrTail: "agent saw it fail"}
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
	})

	if len(vr.Steps) != 1 {
		t.Fatalf("steps = %d, want the rollup synthesised as one step", len(vr.Steps))
	}
	s := vr.Steps[0]
	if s.Repo != "0-acme/api" || s.Command != checkCmd || s.Passed || s.StderrTail != "agent saw it fail" {
		t.Errorf("synthesised step = %+v", s)
	}
	if !s.BaselineRan || s.BaselinePassed || !vr.PreExisting {
		t.Errorf("baseline not established for the synthesised step: %+v pre_existing=%v", s, vr.PreExisting)
	}
}

func TestRollupWithoutStepsIsNotSynthesisedForTwoRepos(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	a, aSHA := newRepo(t, workDir, "0-acme/api", 1)
	b, bSHA := newRepo(t, workDir, "1-acme/web", 1)

	vr := &result.VerifyResult{Ran: true, Passed: false, Command: checkCmd}
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{a.dir: aSHA, b.dir: bSHA},
	})
	if len(vr.Steps) != 0 || vr.PreExisting {
		t.Errorf("a rollup over two repos names neither; got steps=%+v pre_existing=%v", vr.Steps, vr.PreExisting)
	}
}

// The rollup is agent-authored: "passed" with a failed step underneath must
// not pass the gate. The step wins, and it is replayed like any failure.
func TestFailedStepOverridesPassingRollup(t *testing.T) {
	mustGit(t)
	workDir := newWorkDir(t)
	repo, startSHA := newRepo(t, workDir, "0-acme/api", 0)
	repo.writeCheck(1) // the agent's uncommitted change breaks it

	vr := &result.VerifyResult{Ran: true, Passed: true, Command: checkCmd, Steps: []result.VerifyStep{failedStep("0-acme/api")}}
	Annotate(context.Background(), vr, Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{repo.dir: startSHA},
	})
	if vr.Passed {
		t.Error("rollup passed=true survived a failed step")
	}
	if !vr.Steps[0].BaselineRan || !vr.Steps[0].BaselinePassed || vr.PreExisting {
		t.Errorf("the failed step should have been replayed and found new: %+v pre_existing=%v", vr.Steps[0], vr.PreExisting)
	}
}
