package review

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Caps on how much diff a review prompt may carry.
//
// A review that is handed half a million bytes of diff does not review it — it
// skims, and skimming silently is the worst outcome available here. Truncating
// with an explicit marker, and recording the truncation in the coverage reason
// of every pass that ran, at least makes the gap visible in the PR body.
//
// The numbers are set by a HARD LIMIT, not by taste. Every driver passes the
// prompt as a single argv element, and Linux caps one argument at MAX_ARG_STRLEN
// = 128 KiB; an argument over that makes execve fail with E2BIG, so the agent
// never starts at all and a large Step would produce a review that could not
// run. MaxPromptBytes is the whole prompt's ceiling, comfortably under that
// limit, and the diff and spec budgets are carved out of it.
const (
	// MaxPromptBytes caps the ENTIRE review prompt — diff, spec, briefs and
	// boilerplate together. This is the number that keeps execve working.
	MaxPromptBytes = 100000
	// MaxDiffBytes caps the diff portion across every repository.
	MaxDiffBytes = 88000
	// MaxSpecBytes caps the spec the diff is judged against. A spec is
	// usually a few hundred bytes; the cap exists so a pathological one
	// cannot crowd the diff out of the prompt.
	MaxSpecBytes = 8000
	// MaxFileDiffBytes caps one file's hunk, so a single generated file
	// cannot crowd out every other change in the Step.
	MaxFileDiffBytes = 60000
)

// Diff is the change under review: the text handed to the agent, the paths it
// touches (which the cost gate reads), and whether anything was elided.
type Diff struct {
	// Text is the concatenated, capped diff across every repository, with
	// elision markers where content was dropped.
	Text string
	// Paths is every changed path, prefixed with its repository directory
	// so two repos changing "README.md" stay distinguishable.
	Paths []string
	// Truncated is true when either cap fired. Recorded in the coverage
	// reason of every pass that ran — a review of a truncated diff is a
	// partial review and must not be reported as a complete one.
	Truncated bool
}

// Empty reports whether the change under review touches nothing at all.
func (d Diff) Empty() bool {
	return len(d.Paths) == 0 && strings.TrimSpace(d.Text) == ""
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
// Repositories are processed in sorted order so two runs over the same working
// tree produce the same prompt.
func Compute(workDir string, baseCommits map[string]string) (Diff, error) {
	dirs := make([]string, 0, len(baseCommits))
	for dir := range baseCommits {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var out Diff
	var b strings.Builder
	budget := MaxDiffBytes
	overallTruncated := false
	// shown is every changed path whose diff made it into Text, keyed the way
	// out.Paths is. When the overall cap fires, the difference between
	// out.Paths and shown is the list of files the reviewer has to open for
	// itself — see notShownMarker.
	shown := map[string]bool{}

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
		for _, p := range paths {
			out.Paths = append(out.Paths, filepath.Join(dir, p))
		}
		chunks, err := repoDiffChunks(repoPath, base)
		if err != nil {
			return Diff{}, err
		}
		if len(chunks) == 0 {
			continue
		}
		if budget <= 0 {
			overallTruncated = true
			break
		}
		// ONE header per repository. Emitting it per file made the prompt
		// read as though each file were its own repo, and spent the budget
		// on repetition rather than on diff.
		header := fmt.Sprintf("\n--- repository %s (against %s) ---\n", dir, shortSHA(base))
		if len(header) >= budget {
			overallTruncated = true
			break
		}
		b.WriteString(header)
		budget -= len(header)

		for _, chunk := range chunks {
			capped, fileTruncated := capFileChunk(chunk)
			out.Truncated = out.Truncated || fileTruncated
			if budget <= 0 {
				overallTruncated = true
				break
			}
			if len(capped) > budget {
				overallTruncated = true
				b.WriteString(truncateBytesOnRuneBoundary(capped, budget))
				// A file cut mid-diff is still on the page: the reviewer can
				// see its name and the elision marker and open it. Only files
				// that never appear go on the not-shown list.
				markShown(shown, dir, chunk)
				budget = 0
				break
			}
			b.WriteString(capped)
			markShown(shown, dir, chunk)
			budget -= len(capped)
		}
		if budget <= 0 {
			break
		}
	}

	if overallTruncated {
		out.Truncated = true
		b.WriteString(fmt.Sprintf(
			"\n[… diff truncated: the overall cap of %d bytes was reached; later files are not shown]\n",
			MaxDiffBytes))
		b.WriteString(notShownMarker(out.Paths, shown))
	}
	out.Text = b.String()
	return out, nil
}

// maxNotShownListBytes bounds the list of files the cap dropped. It rides
// OUTSIDE MaxDiffBytes — it is the one thing worth spending over budget on,
// because it turns "later files are not shown" from a shrug into a to-do list
// the reviewer can work through with Read and git diff — but it is bounded so
// the whole prompt still clears MaxPromptBytes with the spec and briefs.
const maxNotShownListBytes = 1200

// notShownMarker names the changed files whose diff did not make it into
// Text, so the reviewer knows what to open rather than what it missed. Empty
// when every changed file was shown.
func notShownMarker(paths []string, shown map[string]bool) string {
	var missing []string
	for _, p := range paths {
		if !shown[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Changed files NOT shown above — open them directly with Read or git diff before reporting coverage:")
	listed := 0
	for _, p := range missing {
		entry := " " + p
		if b.Len()+len(entry) > maxNotShownListBytes {
			break
		}
		b.WriteString(entry)
		listed++
	}
	if rest := len(missing) - listed; rest > 0 {
		b.WriteString(fmt.Sprintf(" (and %d more)", rest))
	}
	b.WriteString("]\n")
	return b.String()
}

// markShown records the file a chunk belongs to, keyed like Diff.Paths. A
// chunk's first line is git's "diff --git a/<path> b/<path>" header; the
// b/ side is the path as it exists in the working tree, which is the one the
// reviewer would open. A chunk with no recognisable header marks nothing —
// it is still on the page, and an unknown key would never match a path.
func markShown(shown map[string]bool, dir, chunk string) {
	if path, ok := chunkPath(chunk); ok {
		shown[filepath.Join(dir, path)] = true
	}
}

// chunkPath extracts the b/ path from a chunk's "diff --git" header.
func chunkPath(chunk string) (string, bool) {
	line := chunk
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	const prefix = "diff --git "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := line[len(prefix):]
	i := strings.LastIndex(rest, " b/")
	if i < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[i+len(" b/"):]), true
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

// repoDiffChunks returns the diff of this repository split into per-file
// chunks: the tracked diff against base, then one synthetic chunk per
// untracked file. Splitting per file is what lets MaxFileDiffBytes apply to a
// file rather than to the whole change.
func repoDiffChunks(repoPath, base string) ([]string, error) {
	var chunks []string
	out, err := git(repoPath, "diff", base, "--")
	if err != nil {
		return nil, fmt.Errorf("diffing %s against %s: %w", repoPath, shortSHA(base), err)
	}
	chunks = append(chunks, splitPerFile(out)...)

	untracked, err := untrackedFiles(repoPath)
	if err != nil {
		return nil, err
	}
	for _, path := range untracked {
		out, err := gitDiffNoIndex(repoPath, path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(out) == "" {
			continue
		}
		chunks = append(chunks, out)
	}
	return chunks, nil
}

// splitPerFile cuts a multi-file diff at each "diff --git" header. The header
// line is kept with the chunk that follows it.
func splitPerFile(diff string) []string {
	const marker = "diff --git "
	var chunks []string
	rest := diff
	for {
		idx := strings.Index(rest, marker)
		if idx < 0 {
			if strings.TrimSpace(rest) != "" && len(chunks) == 0 {
				chunks = append(chunks, rest)
			}
			return chunks
		}
		rest = rest[idx:]
		next := strings.Index(rest[len(marker):], "\n"+marker)
		if next < 0 {
			return append(chunks, rest)
		}
		chunks = append(chunks, rest[:len(marker)+next+1])
		rest = rest[len(marker)+next+1:]
	}
}

// capFileChunk truncates one file's diff to MaxFileDiffBytes, leaving a marker
// that says how much was dropped. The marker matters more than the bytes: a
// silently shortened diff reads as a complete one.
//
// The cut lands on a rune boundary: slicing bytes would leave a half-character
// at the seam, and the prompt is JSON-encoded and rendered downstream.
func capFileChunk(chunk string) (string, bool) {
	if len(chunk) <= MaxFileDiffBytes {
		return chunk, false
	}
	kept := truncateBytesOnRuneBoundary(chunk, MaxFileDiffBytes)
	dropped := len(chunk) - len(kept)
	return kept + fmt.Sprintf(
		"\n[… %d bytes of this file's diff elided: the per-file cap of %d bytes was reached]\n",
		dropped, MaxFileDiffBytes), true
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
