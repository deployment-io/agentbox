package review

import (
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
const (
	// MaxDiffBytes caps the whole diff across every repository.
	MaxDiffBytes = 400000
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
// Repositories are processed in sorted order so two runs over the same working
// tree produce the same prompt.
func Compute(workDir string, baseCommits map[string]string) Diff {
	dirs := make([]string, 0, len(baseCommits))
	for dir := range baseCommits {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	var out Diff
	var b strings.Builder
	budget := MaxDiffBytes
	for _, dir := range dirs {
		repoPath := filepath.Join(workDir, dir)
		base := baseCommits[dir]
		paths := changedPaths(repoPath, base)
		for _, p := range paths {
			out.Paths = append(out.Paths, filepath.Join(dir, p))
		}
		for _, chunk := range repoDiffChunks(repoPath, base) {
			capped, fileTruncated := capFileChunk(chunk)
			out.Truncated = out.Truncated || fileTruncated
			header := fmt.Sprintf("\n--- repository %s (against %s) ---\n", dir, shortSHA(base))
			if budget <= 0 {
				out.Truncated = true
				break
			}
			piece := header + capped
			if len(piece) > budget {
				out.Truncated = true
				b.WriteString(piece[:budget])
				b.WriteString("\n[… diff truncated: the overall cap of " + fmt.Sprint(MaxDiffBytes) + " bytes was reached; later files are not shown]\n")
				budget = 0
				break
			}
			b.WriteString(piece)
			budget -= len(piece)
		}
	}
	out.Text = b.String()
	return out
}

// changedPaths lists the paths this repository changed since base: tracked
// changes plus untracked files. Best-effort — a repository whose git commands
// fail contributes nothing rather than failing the review, and the diff text
// is built by the same commands, so a repo that reports no paths also
// contributes no diff.
func changedPaths(repoPath, base string) []string {
	var paths []string
	if out, err := git(repoPath, "diff", "--name-only", base, "--"); err == nil {
		paths = append(paths, nonEmptyLines(out)...)
	}
	paths = append(paths, untrackedFiles(repoPath)...)
	sort.Strings(paths)
	return paths
}

func untrackedFiles(repoPath string) []string {
	out, err := git(repoPath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil
	}
	return nonEmptyLines(out)
}

// repoDiffChunks returns the diff of this repository split into per-file
// chunks: the tracked diff against base, then one synthetic chunk per
// untracked file. Splitting per file is what lets MaxFileDiffBytes apply to a
// file rather than to the whole change.
func repoDiffChunks(repoPath, base string) []string {
	var chunks []string
	if out, err := git(repoPath, "diff", base, "--"); err == nil {
		chunks = append(chunks, splitPerFile(out)...)
	}
	for _, path := range untrackedFiles(repoPath) {
		// --no-index exits 1 when the files differ, which is always true
		// here, so the output is taken regardless of the exit status.
		out, _ := gitAllowFailure(repoPath, "diff", "--no-index", "--", "/dev/null", path)
		if strings.TrimSpace(out) == "" {
			continue
		}
		chunks = append(chunks, out)
	}
	return chunks
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
func capFileChunk(chunk string) (string, bool) {
	if len(chunk) <= MaxFileDiffBytes {
		return chunk, false
	}
	dropped := len(chunk) - MaxFileDiffBytes
	return chunk[:MaxFileDiffBytes] +
		fmt.Sprintf("\n[… %d bytes of this file's diff elided: the per-file cap of %d bytes was reached]\n", dropped, MaxFileDiffBytes), true
}

func git(repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gitAllowFailure runs git and returns whatever it printed even when it exits
// non-zero, for the commands whose non-zero exit IS the expected answer
// (`diff --no-index` exits 1 when the files differ).
func gitAllowFailure(repoPath string, args ...string) (string, error) {
	full := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", full...).Output()
	return string(out), err
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
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
