// Package verify replays a failed verification step on a pristine copy of
// the repository as it stood when agentbox started, so a failure this run
// introduced can be told apart from one it merely inherited.
//
// Without that distinction the runner has one move for any failing verify:
// discard the work. That is the right call for a regression the agent wrote
// and the wrong one for a build that was already red before the agent opened
// the repo — there the whole Step is thrown away over something the agent
// neither caused nor was asked to fix.
//
// Agent-agnostic on purpose: it takes a *result.VerifyResult and a map of
// recorded start commits, and knows nothing about which CLI produced them.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/deployment-io/agentbox/internal/result"
)

// DefaultStepTimeout bounds ONE replay. A baseline run is the same build or
// test suite the agent already ran once, so ten minutes is generous; the cap
// exists because the replay runs after the agent has finished, inside the
// runner's wall-clock budget for the container, and a hung command here
// would burn that budget with nothing to show for it.
const DefaultStepTimeout = 10 * time.Minute

// DefaultTotalBudget bounds ALL replays in a run. Per-step timeouts alone do
// not bound the whole: a multi-repo task with many failed steps could serially
// approach the runner's 4h container cap and turn a diagnostic into an outage.
// Steps that no longer fit report baseline_ran:false, which keeps the gate
// closed rather than guessing.
const DefaultTotalBudget = 30 * time.Minute

// cleanupTimeout bounds worktree teardown. Deliberately run on a FRESH
// context: teardown must happen even when the run context was cancelled
// mid-replay, or a SIGTERM would leave a stray worktree registered in the
// agent's .git and a stray directory under the work dir.
const cleanupTimeout = 30 * time.Second

// killGrace bounds how long a cancelled replay may keep its output pipes
// open after the process group has been killed, so cancellation can't be
// silently converted back into a long wait.
const killGrace = 5 * time.Second

// tmpDirRel is the work-dir-relative scratch directory the baseline
// worktrees are created under. Dot-prefixed, which is what keeps them
// invisible to repository discovery (agent.repoDirsUnder and
// vendoring.BuildPlan both skip dot-prefixed directories) and to the
// runner's per-repo commit diff. It is also the directory the runner
// pre-creates and chowns to the agent user, and it lives on the bind mount
// rather than the small tmpfs.
const tmpDirRel = ".agentbox-tmp"

// tailMaxBytes caps a captured baseline tail. The END is kept — a compiler
// or test runner puts the failure last and the head is progress noise.
const tailMaxBytes = 2000

// depDirs are dependency trees symlinked from the agent's checkout into the
// baseline worktree instead of being reinstalled. Reinstalling would need
// network the agent container does not have, and would take longer than the
// replay itself. The trade is deliberate: a dependency the agent ADDED is
// visible during replay too, so a failure caused purely by a missing package
// can read as pre-existing. That is the cheaper and far more deterministic
// side of the trade — the alternative is a minutes-long install whose own
// failure modes would dominate the signal.
var depDirs = []string{"node_modules", ".venv"}

// Options configures a baseline replay pass.
type Options struct {
	// WorkDir is the directory the repositories are checked out under —
	// the root every step's Repo field is resolved against, and the parent
	// of the scratch directory worktrees are created in.
	WorkDir string

	// StartCommits maps an absolute repository directory to the commit it
	// was checked out at when agentbox started, as recorded BEFORE the
	// agent subprocess ran. A repo missing from this map has no baseline.
	StartCommits map[string]string

	// Log receives the human-readable account of each replay. Nil discards.
	Log io.Writer

	// StepTimeout / TotalBudget override the defaults; zero means default.
	StepTimeout time.Duration
	TotalBudget time.Duration
}

func (o Options) withDefaults() Options {
	if o.StepTimeout <= 0 {
		o.StepTimeout = DefaultStepTimeout
	}
	if o.TotalBudget <= 0 {
		o.TotalBudget = DefaultTotalBudget
	}
	return o
}

func (o Options) logf(format string, args ...any) {
	if o.Log == nil {
		return
	}
	fmt.Fprintf(o.Log, "[agentbox] "+format+"\n", args...)
}

// Annotate replays every FAILED step of vr on its repository's recorded
// start-of-run commit and records what it found on the step, then sets
// vr.PreExisting when every failed step also failed on that baseline.
//
// Mutates vr in place. No-op when the agent ran no verify, when the rollup
// passed, or when there is no per-step detail to act on — in each of those
// cases PreExisting stays false and the consumer's existing behaviour is
// unchanged.
//
// FAILURE-CLOSED throughout: anything that stops a baseline from being
// established (no recorded commit, an unresolvable repo path, a commit that
// is no longer reachable, an errored or timed-out replay) is reported as
// baseline_ran:false and prevents PreExisting from being set. "We could not
// tell" and "the failure is new" must lead to the same outcome, because
// pushing on a wrong guess is the one failure mode worse than discarding
// work.
//
// Honours ctx: a SIGTERM during replay aborts the remaining work promptly
// rather than extending shutdown by the timeout.
func Annotate(ctx context.Context, vr *result.VerifyResult, opts Options) {
	if vr == nil {
		return
	}
	// The whole VerifyResult arrived as JSON the AGENT wrote, so it can
	// contain these fields already — and an agent that emits
	// "pre_existing":true would otherwise walk its own failure straight
	// past the gate. They are agentbox's to state, so wipe them before
	// establishing anything.
	clearBaselineClaims(vr)

	if !vr.Ran {
		return
	}
	// The prompt asks for per-repo steps only when there is more than one
	// repository, so a single-repo run — the common case — reports the
	// legacy rollup alone. That rollup IS the one repository's step; treat
	// it as such, or single-repo Tasks would never get a baseline at all.
	if len(vr.Steps) == 0 && !vr.Passed {
		if step, ok := soleRepoStep(vr, opts); ok {
			vr.Steps = []result.VerifyStep{step}
		}
	}
	// The rollup is agent-authored too. "passed = every step passed" is the
	// contract, so enforce it rather than let a rollup that contradicts its
	// own steps walk a failed step past the gate.
	for i := range vr.Steps {
		if !vr.Steps[i].Passed {
			vr.Passed = false
			break
		}
	}
	if vr.Passed {
		return
	}
	failed := failedStepIndexes(vr.Steps)
	if len(failed) == 0 {
		// Either a legacy single-command payload with no per-step detail, or
		// a rollup that disagrees with its own steps. Nothing to replay and
		// nothing we can claim — leave the gate closed.
		return
	}
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.TotalBudget)

	preExisting := true
	for _, i := range failed {
		if !opts.replayOne(ctx, &vr.Steps[i], deadline) {
			preExisting = false
		}
	}
	vr.PreExisting = preExisting
}

// soleRepoStep turns a rollup with no per-step detail into the single step
// it describes, when exactly one repository was checked out at the start of
// the run. With two or more there is no telling which one the rollup is
// about, so nothing is synthesised and the gate stays closed.
func soleRepoStep(vr *result.VerifyResult, opts Options) (result.VerifyStep, bool) {
	if len(opts.StartCommits) != 1 || strings.TrimSpace(vr.Command) == "" {
		return result.VerifyStep{}, false
	}
	var dir string
	for d := range opts.StartCommits {
		dir = d
	}
	rel, err := filepath.Rel(filepath.Clean(opts.WorkDir), dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return result.VerifyStep{}, false
	}
	return result.VerifyStep{
		Repo:       rel,
		Command:    vr.Command,
		Passed:     vr.Passed,
		StdoutTail: vr.StdoutTail,
		StderrTail: vr.StderrTail,
	}, true
}

// clearBaselineClaims drops every field this package owns, so nothing the
// agent asserted about a baseline survives into result.json unverified.
func clearBaselineClaims(vr *result.VerifyResult) {
	vr.PreExisting = false
	for i := range vr.Steps {
		vr.Steps[i].BaselineRan = false
		vr.Steps[i].BaselinePassed = false
		vr.Steps[i].BaselineStderrTail = ""
	}
}

// replayOne establishes the baseline for a single failed step, writing the
// outcome onto it. Returns whether the step counts toward pre-existing —
// true only when the baseline ran AND failed.
func (o Options) replayOne(ctx context.Context, step *result.VerifyStep, deadline time.Time) bool {
	label := step.Repo
	if label == "" {
		label = "(unnamed repo)"
	}

	repoDir, sha, err := o.baselineFor(step.Repo)
	if err != nil {
		o.logf("verify: %s failed; no base commit available (%v) — treating failure as new", label, err)
		return false
	}
	o.logf("verify: %s failed; replaying on base commit %s", label, sha)

	ran, passed, tail, err := o.run(ctx, repoDir, sha, step.Command, deadline)
	step.BaselineRan = ran
	step.BaselinePassed = passed
	if ran && !passed {
		step.BaselineStderrTail = tail
	}
	switch {
	case !ran:
		o.logf("verify: %s base replay did not complete (%v) — treating failure as new", label, err)
		return false
	case passed:
		o.logf("verify: %s passes on base; failure is new", label)
		return false
	default:
		o.logf("verify: %s fails on base too (pre-existing)", label)
		return true
	}
}

// baselineFor resolves a step's repo field to the checkout directory and the
// commit that directory was at when the run began. Errors — never a silent
// zero value — so the caller can say why no baseline was available.
func (o Options) baselineFor(repo string) (repoDir, sha string, err error) {
	repoDir, err = resolveRepoDir(o.WorkDir, repo)
	if err != nil {
		return "", "", err
	}
	sha = o.StartCommits[repoDir]
	if sha == "" {
		// Not a checkout when the run began: a directory that appeared
		// mid-run, an unborn HEAD, or a rev-parse that failed.
		return "", "", fmt.Errorf("no start-of-run commit recorded for %q", repo)
	}
	return repoDir, sha, nil
}

// resolveRepoDir maps a step's repo field — a path relative to the work dir,
// as the agent sees it — to an absolute directory under the work dir.
// Absolute paths and anything that escapes the work dir are rejected rather
// than clamped: the field is agent-authored text, and a replay is a command
// execution, so the only safe reading of an unexpected shape is "no
// baseline".
func resolveRepoDir(workDir, repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", errors.New("step names no repo")
	}
	if filepath.IsAbs(repo) {
		return "", fmt.Errorf("repo %q must be relative to the work dir", repo)
	}
	base := filepath.Clean(workDir)
	dir := filepath.Clean(filepath.Join(base, repo))
	if dir == base || !strings.HasPrefix(dir, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("repo %q does not resolve under the work dir", repo)
	}
	return dir, nil
}

// run executes the step's command inside a pristine worktree of sha.
//
// Returns ran=false for everything that is not a clean verdict: an empty
// command, an exhausted budget, a worktree that could not be created, a
// process that could not be started, or a run that was cancelled or timed
// out. A non-zero EXIT is a verdict (ran=true, passed=false) — that is the
// pre-existing-failure case this whole package exists to detect.
func (o Options) run(ctx context.Context, repoDir, sha, command string, deadline time.Time) (ran, passed bool, tail string, err error) {
	if strings.TrimSpace(command) == "" {
		return false, false, "", errors.New("step names no command")
	}
	if err := ctx.Err(); err != nil {
		return false, false, "", err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, false, "", fmt.Errorf("replay budget of %s exhausted", o.TotalBudget)
	}
	timeout := o.StepTimeout
	if remaining < timeout {
		timeout = remaining
	}

	wt, cleanup, err := o.addWorktree(ctx, repoDir, sha)
	if err != nil {
		return false, false, "", err
	}
	defer cleanup()
	linkDeps(repoDir, wt)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// The container's shell, matching how the agent ran the command: the
	// contract asks for a single command line, which may well contain && or
	// a pipeline.
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = wt
	cmd.Env = replayEnv()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A build or test command is a process TREE (the shell, a toolchain, its
	// test binaries). Killing only the shell leaves the children running and
	// holding the output pipes, so Wait blocks until they finish on their own
	// — which is exactly the timeout the cancellation was meant to cut short.
	// Own process group + group kill + a bounded drain makes cancellation
	// actually prompt.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = killGrace
	runErr := cmd.Run()

	if ctxErr := runCtx.Err(); ctxErr != nil {
		// Cancelled (SIGTERM) or past the per-step / budget cap. Whatever the
		// command had produced so far says nothing about the baseline.
		return false, false, "", ctxErr
	}
	if runErr == nil {
		return true, true, "", nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return true, false, pickTail(stderr.String(), stdout.String()), nil
	}
	return false, false, "", runErr
}

// addWorktree materialises sha as a detached worktree under the work dir's
// scratch directory, returning its path and a teardown func.
//
// `git worktree add --detach <path> <sha>` — never HEAD, and never git
// stash. HEAD is wrong because an agent that commits its own work moves it,
// so replaying there would reproduce the agent's OWN failure and call it
// pre-existing. stash is wrong because it mutates the tree the runner is
// about to commit from.
func (o Options) addWorktree(ctx context.Context, repoDir, sha string) (string, func(), error) {
	base := filepath.Join(filepath.Clean(o.WorkDir), tmpDirRel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("create replay scratch dir: %w", err)
	}
	dir, err := os.MkdirTemp(base, "verify-baseline-")
	if err != nil {
		return "", nil, fmt.Errorf("create replay scratch dir: %w", err)
	}
	// git refuses to add a worktree at an existing path, so name a child of
	// the temp dir and let git create it.
	wt := filepath.Join(dir, "repo")
	if out, err := runGit(ctx, repoDir, "worktree", "add", "--detach", wt, sha); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("git worktree add %s: %w: %s", sha, err, out)
	}
	cleanup := func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = runGit(cleanCtx, repoDir, "worktree", "remove", "--force", wt)
		_, _ = runGit(cleanCtx, repoDir, "worktree", "prune")
		_ = os.RemoveAll(dir)
	}
	return wt, cleanup, nil
}

// linkDeps symlinks the agent's installed dependency trees into the baseline
// worktree. Best-effort: a missing tree (or a link that can't be made) just
// means the replay runs without it, which the command itself will report.
func linkDeps(repoDir, wt string) {
	for _, name := range depDirs {
		src := filepath.Join(repoDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(wt, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		_ = os.Symlink(src, dst)
	}
}

// replayEnv is the agent's own environment — so the shared module caches
// (GOMODCACHE / GOCACHE / GOPRIVATE) and the egress-proxy vars the agent had
// are inherited verbatim — plus GOWORK=off.
//
// GOWORK is forced because the baseline worktree sits under the work dir,
// outside whatever workspace a parent go.work describes, and an inherited
// workspace would resolve the module's own packages back to the AGENT'S
// checkout — quietly replaying the agent's code on the base commit's tree.
func replayEnv() []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "GOWORK=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOWORK=off")
}

func runGit(ctx context.Context, repoDir string, args ...string) (string, error) {
	full := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

// failedStepIndexes returns the indexes of steps that failed. Only those are
// replayed — a passing step has nothing to compare against.
func failedStepIndexes(steps []result.VerifyStep) []int {
	var out []int
	for i := range steps {
		if !steps[i].Passed {
			out = append(out, i)
		}
	}
	return out
}

// pickTail prefers stderr (where build and test failures land) and falls back
// to stdout (where some tools report them instead), bounded from the end.
func pickTail(stderr, stdout string) string {
	tail := strings.TrimSpace(stderr)
	if tail == "" {
		tail = strings.TrimSpace(stdout)
	}
	if len(tail) <= tailMaxBytes {
		return tail
	}
	tail = tail[len(tail)-tailMaxBytes:]
	// Don't start mid-rune: the cut is byte-based and the output is
	// arbitrary text.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return "…" + tail
}
