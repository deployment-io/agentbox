// Package repogit is `agentbox repo-git <repository directory> <subcommand>
// [arguments…]`: a fixed set of read-only git subcommands run on one
// repository under the work directory.
//
// A read-only Claude Code session works in /work, which is not a repository,
// and its allowlist has no way to point git at one: `cd <repo> && git …` is
// refused by Claude Code itself and `git -C <repo> …` matches no entry. This
// command is the one route it is given ("Bash(agentbox repo-git *)" in
// internal/claude), so everything it runs is held to what reading needs:
//
//   - only log, diff, show, blame and status;
//   - only a real repository directory inside the work directory, reached
//     without a symbolic link, whose .git is a directory;
//   - no argument that writes a file (--output) or runs a program
//     (--ext-diff, --textconv), however abbreviated;
//   - git by absolute path, under an environment built from scratch, with -c
//     settings that close hooks, fsmonitor, the pager and every transport;
//   - and only a repository whose configuration — and that of every
//     populated submodule, which status and diff look into, and of every
//     submodule git directory under .git/modules, which --submodule=diff
//     opens even without a working tree — holds nothing
//     beyond what a runner clone writes, so a filter driver, an include, a
//     credential helper or any other key that could start a program is
//     refused before git runs.
package repogit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitPath is git by absolute path, never looked up on the caller's PATH.
const gitPath = "/usr/bin/git"

// Usage is the form a usage refusal names.
const Usage = "usage: agentbox repo-git <repository directory> <log|diff|show|blame|status> [arguments…]"

// subcommandFlags are the allowed subcommands, each with the flags that go
// right after it to keep external diff programs and textconv filters from
// running whatever the repository's configuration says.
var subcommandFlags = map[string][]string{
	"log":    {"--no-ext-diff", "--no-textconv"},
	"diff":   {"--no-ext-diff", "--no-textconv"},
	"show":   {"--no-ext-diff", "--no-textconv"},
	"blame":  {"--no-textconv"},
	"status": nil,
}

// refusedOptions are the long options refused wherever they appear, by any
// unambiguous abbreviation git's option parser could accept.
var refusedOptions = []string{"--output", "--ext-diff", "--textconv"}

// overrides are the -c settings every git invocation carries.
var overrides = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "protocol.allow=never",
}

// Env is the whole environment git runs under; nothing is inherited.
func Env() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
		"LC_ALL=C.UTF-8",
		"TERM=dumb",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
}

// refusal is a reason repo-git won't run git; it is printed as one
// "[repo-git] <reason>" line and the command exits 2.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

func refuse(format string, a ...any) error {
	return &refusal{reason: fmt.Sprintf(format, a...)}
}

// Main runs `agentbox repo-git` with args (everything after "repo-git") and
// returns the exit code: git's own, or 2 for a refusal.
// stdin is passed to git (log --stdin, blame --contents -).
func Main(args []string, workDir string, stdin io.Reader, stdout, stderr io.Writer) int {
	repo, sub, rest, err := Parse(args, workDir)
	if err == nil {
		err = checkConfig(repo, "this repository", false)
	}
	if err == nil {
		err = checkSubmodules(repo, "", 0)
	}
	if err == nil {
		err = checkModules(filepath.Join(repo, ".git", "modules"), ".git/modules", 0)
	}
	if err != nil {
		fmt.Fprintf(stderr, "[repo-git] %s\n", oneLine(err.Error()))
		return 2
	}
	return run(GitArgs(repo, sub, rest), stdin, stdout, stderr)
}

// oneLine escapes control characters (a newline in a refused argument or
// path, say) so a refusal stays the one line it is meant to be.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// Parse validates the command line against workDir and returns the
// repository's absolute directory, the subcommand and its arguments.
func Parse(args []string, workDir string) (repo, sub string, rest []string, err error) {
	if len(args) < 2 {
		return "", "", nil, refuse("missing repository directory or subcommand; %s", Usage)
	}
	sub = args[1]
	if _, ok := subcommandFlags[sub]; !ok {
		return "", "", nil, refuse("subcommand %q isn't allowed (only log, diff, show, blame and status); %s", sub, Usage)
	}
	repo, err = repoDir(args[0], workDir)
	if err != nil {
		return "", "", nil, err
	}
	rest = args[2:]
	if err := checkArgs(rest); err != nil {
		return "", "", nil, err
	}
	return repo, sub, rest, nil
}

// repoDir resolves dir (relative to workDir, or absolute inside it) and
// checks it is a repository directory repo-git may read: inside workDir after
// cleaning, no symbolic link below workDir, and a .git that is a directory.
func repoDir(dir, workDir string) (string, error) {
	if dir == "" {
		return "", refuse("missing repository directory; %s", Usage)
	}
	base, err := filepath.Abs(workDir)
	if err != nil {
		return "", refuse("the work directory %q can't be resolved: %v", workDir, err)
	}
	path := dir
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(base, path)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return "", refuse("%s isn't inside the work directory %s", dir, base)
	}
	cur := base
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			info, err := os.Lstat(cur)
			if err != nil {
				return "", refuse("%s: %v", dir, errors.Unwrap(err))
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return "", refuse("%s: %s is a symbolic link, which repo-git doesn't follow", dir, cur)
			}
			if !info.IsDir() {
				return "", refuse("%s: %s isn't a directory", dir, cur)
			}
		}
	}
	info, err := os.Lstat(filepath.Join(path, ".git"))
	switch {
	case err != nil:
		return "", refuse("%s isn't a repository directory (it has no .git directory)", dir)
	case info.Mode()&fs.ModeSymlink != 0:
		return "", refuse("%s/.git is a symbolic link, which repo-git doesn't follow", dir)
	case !info.IsDir():
		return "", refuse("%s/.git isn't a directory (a submodule or a linked worktree), which repo-git doesn't read", dir)
	}
	return path, nil
}

// checkArgs refuses --output, --ext-diff and --textconv wherever they appear,
// including after "--", by their name (the argument up to its first "=")
// being a non-empty prefix of one of them — except exactly --text.
func checkArgs(args []string) error {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if len(name) <= 2 || name == "--text" {
			continue
		}
		for _, opt := range refusedOptions {
			if strings.HasPrefix(opt, name) {
				return refuse("the argument %s (%s) isn't allowed: it writes a file or runs a program", a, opt)
			}
		}
	}
	return nil
}

// GitArgs is the argv (without the binary) that runs sub on repo.
func GitArgs(repo, sub string, rest []string) []string {
	args := []string{"--no-pager", "--no-optional-locks", "-C", repo}
	args = append(args, overrides...)
	args = append(args, sub)
	args = append(args, subcommandFlags[sub]...)
	return append(args, rest...)
}

// ConfigArgs is the argv (without the binary) that lists the configuration
// git would use for repo, under the same -c settings.
func ConfigArgs(repo string) []string {
	args := []string{"--no-pager", "--no-optional-locks", "-C", repo}
	args = append(args, overrides...)
	return append(args, "config", "--list", "--null", "--show-scope")
}

// GitDirConfigArgs is ConfigArgs for a git directory named with --git-dir
// and --work-tree rather than -C: a submodule's git directory under
// .git/modules has a core.worktree that git would change into, and that
// working tree may be missing. Listing the configuration doesn't read the
// working tree, so the git directory itself stands in for it.
func GitDirConfigArgs(gitDir string) []string {
	args := []string{"--no-pager", "--no-optional-locks", "--git-dir=" + gitDir, "--work-tree=" + gitDir}
	args = append(args, overrides...)
	return append(args, "config", "--list", "--null", "--show-scope")
}

// allowedKeys are the local keys a runner clone writes, lower-cased.
var allowedKeys = map[string]bool{
	"core.bare":                    true,
	"core.repositoryformatversion": true,
	"core.filemode":                true,
	"core.logallrefupdates":        true,
	"core.ignorecase":              true,
	"core.precomposeunicode":       true,
	"core.symlinks":                true,
	"extensions.objectformat":      true,
}

// allowedNamedKeys are the <section>.<name>.<variable> keys a runner clone
// writes, by section, each with its variables (lower-cased).
var allowedNamedKeys = map[string][]string{
	"remote":    {"url", "fetch"},
	"branch":    {"remote", "merge"},
	"submodule": {"url", "active", "path", "branch"},
}

// keyAllowed reports whether a local-scope key is one a runner clone writes.
// Section and variable names compare case-insensitively, as git does. A
// submodule's configuration may also have core.worktree, which git and go-git
// write for a submodule whose git directory is under .git/modules.
func keyAllowed(key string, submodule bool) bool {
	lower := strings.ToLower(key)
	if allowedKeys[lower] || submodule && lower == "core.worktree" {
		return true
	}
	section, after, ok := strings.Cut(key, ".")
	if !ok {
		return false
	}
	i := strings.LastIndex(after, ".")
	if i <= 0 {
		return false
	}
	variable := strings.ToLower(after[i+1:])
	for _, v := range allowedNamedKeys[strings.ToLower(section)] {
		if v == variable {
			return true
		}
	}
	return false
}

// checkConfig reads the configuration git would use for repo and refuses
// unless every local key is on the runner-clone list and nothing comes from
// any scope other than local and command (repo-git's own -c settings). what
// names the repository in a refusal.
func checkConfig(repo, what string, submodule bool) error {
	return checkConfigWith(ConfigArgs(repo), what, submodule)
}

// checkConfigWith is checkConfig with the argv that lists the configuration.
func checkConfigWith(args []string, what string, submodule bool) error {
	cmd := exec.Command(gitPath, args...)
	cmd.Env = Env()
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return refuse("couldn't read %s's git configuration: %s", what, msg)
	}
	return checkConfigList(out.Bytes(), what, submodule)
}

// maxSubmoduleDepth bounds how deep checkSubmodules follows nested
// submodules.
const maxSubmoduleDepth = 8

// checkSubmodules checks the configuration of every populated submodule of
// repo, and theirs in turn. git status and a working-tree diff look into a
// populated submodule with a child git that reads the submodule's own
// configuration (and a diff with --submodule=diff runs one without
// --no-ext-diff or --no-textconv), so that configuration is held to the same
// list as the repository's. prefix is repo's path below the repository
// repo-git was given, for messages.
func checkSubmodules(repo, prefix string, depth int) error {
	cmd := exec.Command(gitPath, LsFilesArgs(repo)...)
	cmd.Env = Env()
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(errOut.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return refuse("couldn't list the submodules of %s: %s", describe(prefix), msg)
	}
	for _, record := range strings.Split(out.String(), "\x00") {
		meta, path, ok := strings.Cut(record, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		name := filepath.ToSlash(filepath.Join(prefix, path))
		dir, populated, err := submoduleDir(repo, path, name)
		if err != nil {
			return err
		}
		if !populated {
			continue
		}
		if depth >= maxSubmoduleDepth {
			return refuse("submodule %s is nested more than %d deep, which repo-git doesn't follow", name, maxSubmoduleDepth)
		}
		if err := checkConfig(dir, "submodule "+name, true); err != nil {
			return err
		}
		if err := checkSubmodules(dir, name, depth+1); err != nil {
			return err
		}
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			if err := checkModules(filepath.Join(dir, ".git", "modules"), name+"/.git/modules", 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkModules checks the configuration of every submodule git directory
// under modules (a repository's .git/modules), populated or not, and of the
// ones nested under theirs. A diff or log with --submodule=diff opens a
// submodule's git directory there even when its working tree is missing,
// and runs a child git under that directory's configuration. A module name
// may contain slashes, so a directory that isn't a git directory is walked
// into. A symbolic link is refused rather than followed. what names modules
// in messages.
func checkModules(modules, what string, depth int) error {
	entries, err := os.ReadDir(modules)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return refuse("%s: %v", what, errors.Unwrap(err))
	}
	if depth >= maxSubmoduleDepth {
		return refuse("%s is nested more than %d deep, which repo-git doesn't follow", what, maxSubmoduleDepth)
	}
	for _, e := range entries {
		dir := filepath.Join(modules, e.Name())
		name := what + "/" + e.Name()
		if e.Type()&fs.ModeSymlink != 0 {
			return refuse("%s is a symbolic link, which repo-git doesn't follow", name)
		}
		if !e.IsDir() {
			continue
		}
		if !isGitDir(dir) {
			if err := checkModules(dir, name, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := checkConfigWith(GitDirConfigArgs(dir), "submodule repository "+name, true); err != nil {
			return err
		}
		if err := checkModules(filepath.Join(dir, "modules"), name+"/modules", depth+1); err != nil {
			return err
		}
	}
	return nil
}

// isGitDir reports whether dir looks like a git directory to git: HEAD, an
// objects directory and a refs directory.
func isGitDir(dir string) bool {
	for _, part := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(dir, part)); err != nil {
			return false
		}
	}
	return true
}

// submoduleDir returns the directory of the submodule at path (relative to
// repo) and whether it is populated (has a .git). A symbolic link on the way
// is refused rather than followed.
func submoduleDir(repo, path, name string) (string, bool, error) {
	cur := repo
	for _, part := range strings.Split(filepath.FromSlash(path), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, refuse("submodule %s: %v", name, errors.Unwrap(err))
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", false, refuse("submodule %s: %s is a symbolic link, which repo-git doesn't follow", name, cur)
		}
		if !info.IsDir() {
			return "", false, nil
		}
	}
	if _, err := os.Lstat(filepath.Join(cur, ".git")); err != nil {
		return "", false, nil
	}
	return cur, true, nil
}

func describe(prefix string) string {
	if prefix == "" {
		return "this repository"
	}
	return "submodule " + prefix
}

// LsFilesArgs is the argv (without the binary) that lists repo's index,
// where submodules appear as mode 160000 entries.
func LsFilesArgs(repo string) []string {
	args := []string{"--no-pager", "--no-optional-locks", "-C", repo}
	args = append(args, overrides...)
	return append(args, "ls-files", "--stage", "-z")
}

// checkConfigList checks `git config --list --null --show-scope` output:
// records of scope NUL key [LF value] NUL.
func checkConfigList(out []byte, what string, submodule bool) error {
	fields := strings.Split(string(out), "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	if len(fields)%2 != 0 {
		return refuse("couldn't read %s's git configuration: unexpected output from git config", what)
	}
	for i := 0; i < len(fields); i += 2 {
		scope := fields[i]
		key, _, _ := strings.Cut(fields[i+1], "\n")
		switch scope {
		case "command":
		case "local":
			if !keyAllowed(key, submodule) {
				return refuse("%s's git configuration has %s, which repo-git doesn't allow", what, key)
			}
		default:
			return refuse("%s's git configuration has %s (%s scope), which repo-git doesn't allow", what, key, scope)
		}
	}
	return nil
}

// run runs git with args under Env and returns its exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(gitPath, args...)
	cmd.Env = Env()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 1
	default:
		fmt.Fprintf(stderr, "[repo-git] couldn't run git: %v\n", err)
		return 2
	}
}
