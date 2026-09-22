package review

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DirName is the directory under the work dir that holds the diff files a
// review round reads. It sits BESIDE the repository checkouts, never inside
// one, so the files are invisible to every repository's diff and can never be
// committed by a fix run. Written by Write at the start of a round, removed by
// Cleanup when the round ends.
const DirName = ".review"

// MaxSpecBytes caps the spec the diff is judged against. A spec is usually a
// few hundred bytes; the cap exists so a pathological one cannot crowd the
// change index out of the prompt.
const MaxSpecBytes = 8000

// MaxIndexPathsPerRepo caps how many changed paths the prompt lists per
// repository. The list is a map of the change, not the change itself — the
// diff file carries every path — so a Step that touches thousands of files
// gets a bounded index and a pointer, not a prompt the size of the change.
const MaxIndexPathsPerRepo = 200

// Diff is the change under review: one COMPLETE diff per repository, handed to
// the reviewer as a file it reads rather than as text folded into its prompt,
// plus the paths the cost gate reads.
//
// Files, not prompt text, because a prompt has a size and a diff does not: any
// cap on inlined diff text drops part of the change on the floor, and a review
// of part of a change reported as a review of the change is the one outcome
// this stage must not produce. A file has no cap. The reviewer reads it with
// the same tool it reads the repositories with, in pages if it must, and
// decides for itself what to skip — a lock file, say — rather than having that
// decided for it by the order the bytes happened to arrive in.
type Diff struct {
	// Paths is every changed path, prefixed with its repository directory
	// so two repos changing "README.md" stay distinguishable. The cost gate
	// reads this list; it is complete regardless of size.
	Paths []string
	// Repos is one entry per repository with a change, sorted by directory
	// so two runs over the same tree produce the same prompt.
	Repos []RepoDiff
}

// RepoDiff is one repository's part of the change.
type RepoDiff struct {
	// Dir is the repository directory relative to the work dir.
	Dir string
	// Base is the commit the diff is against — the commit the repository was
	// checked out at when the Step began.
	Base string
	// Paths is every changed path relative to the repository, sorted.
	Paths []string
	// Text is the complete diff: tracked changes against Base, then one
	// diff-against-nothing per untracked file.
	Text string
	// File is the absolute path the diff was written to. Empty until Write.
	File string
}

// Empty reports whether the change under review touches nothing at all.
func (d Diff) Empty() bool {
	return len(d.Paths) == 0
}

// Compute builds the diff of each repository's WORKING TREE against the commit
// it was checked out at when the Step began.
//
// The baseline is the recorded start commit and never HEAD. An implementer
// committing its own work is explicitly supported (see
// internal/agent/basecommits.go), and on that path HEAD already contains
// everything the agent did — so a diff against it would be empty and the
// review would pass a change it never saw.
//
// Untracked files are included: a new file is the most reviewable kind of
// change there is, and `git diff` alone would not mention it.
//
// EVERY GIT FAILURE IS RETURNED, never swallowed. A base commit this clone does
// not have, or a repository key that names a directory that is not a checkout,
// used to yield an empty or partial diff — which the passes then reviewed and
// reported clean. "I could not see the change" and "the change is fine" must
// never be the same answer, so each base commit is verified up front with
// `git rev-parse --verify <sha>^{commit}` and any git error fails the round.
//
// Nothing here is capped. The diff goes to a file, and the cost gate needs
// every path, so every repository is read in full.
func Compute(workDir string, baseCommits map[string]string) (Diff, error) {
	dirs := make([]string, 0, len(baseCommits))
	for dir := range baseCommits {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var out Diff
	for _, dir := range dirs {
		repoPath := filepath.Join(workDir, dir)
		base := baseCommits[dir]
		if err := verifyBaseCommit(repoPath, base); err != nil {
			return Diff{}, err
		}
		paths, err := changedPaths(repoPath, base)
		if err != nil {
			return Diff{}, err
		}
		if len(paths) == 0 {
			continue
		}
		text, err := repoDiffText(repoPath, base)
		if err != nil {
			return Diff{}, err
		}
		for _, p := range paths {
			out.Paths = append(out.Paths, filepath.Join(dir, p))
		}
		out.Repos = append(out.Repos, RepoDiff{Dir: dir, Base: base, Paths: paths, Text: text})
	}
	return out, nil
}

// Write puts each repository's diff at <workDir>/.review/<dir>.diff and
// records the path on the RepoDiff. The directory is replaced wholesale, so a
// file a previous round left behind — a round killed before Cleanup ran, or a
// repository that has since stopped changing — cannot be mistaken for this
// round's change.
func Write(workDir string, d *Diff) error {
	dir := filepath.Join(workDir, DirName)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing %s: %w", dir, err)
	}
	for i := range d.Repos {
		r := &d.Repos[i]
		file := filepath.Join(dir, r.Dir+".diff")
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(file), err)
		}
		if err := os.WriteFile(file, []byte(r.Text), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", file, err)
		}
		r.File = file
	}
	return nil
}

// Cleanup removes the diff directory. Safe to call when nothing was written.
func Cleanup(workDir string) error {
	return os.RemoveAll(filepath.Join(workDir, DirName))
}

// verifyBaseCommit fails fast when the recorded baseline is not a commit this
// clone has. Without this check `git diff <unknown>` errors, the error is
// discarded, and the review runs on an empty diff and reports it clean.
func verifyBaseCommit(repoPath, base string) error {
	// Deliberately NOT --quiet: git's own stderr ("not a git repository",
	// "Needed a single revision") is the only text that says which of the two
	// ways this can fail actually happened.
	if _, err := git(repoPath, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return fmt.Errorf("repository %s has no commit %s to review against: %w", repoPath, shortSHA(base), err)
	}
	return nil
}

// changedPaths lists the paths this repository changed since base: tracked
// changes plus untracked files.
//
// -z output is NUL-separated and never quoted, so a path with a non-ASCII or
// shell-special character arrives intact. Without it git renders such a path as
// an octal-escaped, double-quoted string, which then fails every cost-gate
// extension check and diffs under the wrong name.
func changedPaths(repoPath, base string) ([]string, error) {
	out, err := git(repoPath, "diff", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, fmt.Errorf("listing changed paths in %s: %w", repoPath, err)
	}
	paths := nulFields(out)
	untracked, err := untrackedFiles(repoPath)
	if err != nil {
		return nil, err
	}
	paths = append(paths, untracked...)
	sort.Strings(paths)
	return paths, nil
}

func untrackedFiles(repoPath string) ([]string, error) {
	out, err := git(repoPath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("listing untracked files in %s: %w", repoPath, err)
	}
	return nulFields(out), nil
}

// repoDiffText returns the complete diff of this repository: the tracked diff
// against base, then one synthetic diff per untracked file.
func repoDiffText(repoPath, base string) (string, error) {
	var b strings.Builder
	out, err := git(repoPath, "diff", base, "--")
	if err != nil {
		return "", fmt.Errorf("diffing %s against %s: %w", repoPath, shortSHA(base), err)
	}
	b.WriteString(out)

	untracked, err := untrackedFiles(repoPath)
	if err != nil {
		return "", err
	}
	for _, path := range untracked {
		out, err := gitDiffNoIndex(repoPath, path)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(out) == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

// truncateBytesOnRuneBoundary returns the longest prefix of s that is at most
// max bytes and does not split a multi-byte rune.
func truncateBytesOnRuneBoundary(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	// A continuation byte is 10xxxxxx; back up until the byte at cut starts
	// a rune (or we reach the beginning).
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// git runs a read-only git command and returns its stdout. core.quotePath=false
// keeps non-ASCII paths readable and unescaped everywhere git prints one.
func git(repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath, "-c", "core.quotePath=false"}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", gitError(err)
	}
	return string(out), nil
}

// gitDiffNoIndex renders an untracked file as a diff against /dev/null.
// `diff --no-index` exits 1 when the two inputs differ, which is always true
// here, so exit 1 is the SUCCESS case; any other failure is a real error and is
// returned rather than swallowed.
func gitDiffNoIndex(repoPath, path string) (string, error) {
	full := []string{"-C", repoPath, "-c", "core.quotePath=false", "diff", "--no-index", "--", "/dev/null", path}
	out, err := exec.Command("git", full...).Output()
	if err == nil {
		return string(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return string(out), nil
	}
	return "", fmt.Errorf("diffing untracked file %s in %s: %w", path, repoPath, gitError(err))
}

// gitError folds git's stderr into the error. Without it every git failure
// reads as a bare "exit status 128", which names nothing a reader can act on.
func gitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
			return fmt.Errorf("%w: %s", err, firstLine(detail))
		}
	}
	return err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// nulFields splits git's -z output on NUL, dropping the trailing empty field.
func nulFields(s string) []string {
	var out []string
	for _, field := range strings.Split(s, "\x00") {
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
