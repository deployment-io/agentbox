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
//   - no argument that writes a file (--output), runs a program (--ext-diff,
//     --textconv, --show-signature), writes objects (--remerge-diff,
//     --diff-merges) or opens submodules again (--ignore-submodules), however
//     abbreviated;
//   - git by absolute path, under an environment built from scratch, with -c
//     settings that close hooks, fsmonitor, the pager, every transport and
//     every signature-verification program;
//   - submodules never opened: status, diff, log and show run with
//     --ignore-submodules=all, and status's -v/--verbose (whose diff doesn't
//     carry it) is refused, so no child git starts under a submodule's own
//     configuration;
//   - and only a repository whose configuration holds nothing beyond what a
//     runner clone writes, so a filter driver, an include, a credential
//     helper or any other key that could start a program is refused before
//     git runs.
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
// right after it: --ignore-submodules=all keeps git from opening a submodule
// (a child git there reads the submodule's own configuration), and
// --no-ext-diff --no-textconv keep external diff programs and textconv
// filters from running whatever the repository's configuration says.
var subcommandFlags = map[string][]string{
	"log":    {"--ignore-submodules=all", "--no-ext-diff", "--no-textconv"},
	"diff":   {"--ignore-submodules=all", "--no-ext-diff", "--no-textconv"},
	"show":   {"--ignore-submodules=all", "--no-ext-diff", "--no-textconv"},
	"blame":  {"--no-textconv"},
	"status": {"--ignore-submodules=all"},
}

// refusedOptions are the long options refused wherever they appear, by any
// unambiguous abbreviation git's option parser could accept: --output writes
// a file, --ext-diff and --textconv run programs, --remerge-diff and
// --diff-merges (=remerge) write temporary objects under .git/objects,
// --show-signature runs gpg, and a later --ignore-submodules or
// --no-ignore-submodules (status) would undo the --ignore-submodules=all
// repo-git puts after the subcommand.
var refusedOptions = []string{
	"--output", "--ext-diff", "--textconv",
	"--remerge-diff", "--diff-merges", "--show-signature",
	"--ignore-submodules", "--no-ignore-submodules",
}

// statusShortValued are status's short options whose (optional) value is
// stuck to them, ending a bundle: -u<mode>, -M<n>.
const statusShortValued = "uM"

// status -v/-vv builds its own diff options for the diff it prints, and
// they don't carry --ignore-submodules=all: with a pathspec, -vv's
// working-tree diff starts a child git in a populated submodule (whose
// .gitmodules ignore setting --ignore-submodules=all would have
// overridden). So status refuses --verbose by the same name/prefix rule and
// -v in any bundle of short options; diff and diff --cached show the same.
const statusVerbose = "--verbose"

// noGPG is the program every gpg.*program setting names, so verifying a
// signature (--format's %G placeholders, log.showSignature) runs nothing
// and the signature shows as unverifiable.
const noGPG = "/nonexistent/repo-git-no-gpg"

// overrides are the -c settings every git invocation carries.
var overrides = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "protocol.allow=never",
	"-c", "gpg.program=" + noGPG,
	"-c", "gpg.openpgp.program=" + noGPG,
	"-c", "gpg.x509.program=" + noGPG,
	"-c", "gpg.ssh.program=" + noGPG,
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
		err = checkConfig(repo)
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
	if sub == "status" {
		if err := checkStatusArgs(rest); err != nil {
			return "", "", nil, err
		}
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

// checkArgs refuses refusedOptions wherever they appear, including after
// "--", by their name (the argument up to its first "=") being a non-empty
// prefix of one of them — except exactly --text.
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
				return refuse("the argument %s (%s) isn't allowed: it writes files or objects, runs a program or reopens submodules", a, opt)
			}
		}
	}
	return nil
}

// checkStatusArgs refuses status's verbose option wherever it appears:
// --verbose or a non-empty prefix of it, and -v in a bundle of short
// options (up to a -u or -M, whose value runs to the bundle's end).
func checkStatusArgs(args []string) error {
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			name, _, _ := strings.Cut(a, "=")
			if len(name) > 2 && strings.HasPrefix(statusVerbose, name) {
				return refuse("the argument %s (%s) isn't allowed with status: its diff would open submodules; use diff or diff --cached", a, statusVerbose)
			}
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		for _, c := range a[1:] {
			if strings.ContainsRune(statusShortValued, c) {
				break
			}
			if c == 'v' {
				return refuse("the argument %s (-v) isn't allowed with status: its diff would open submodules; use diff or diff --cached", a)
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
// Section and variable names compare case-insensitively, as git does.
func keyAllowed(key string) bool {
	if allowedKeys[strings.ToLower(key)] {
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
// any scope other than local and command (repo-git's own -c settings).
func checkConfig(repo string) error {
	cmd := exec.Command(gitPath, ConfigArgs(repo)...)
	cmd.Env = Env()
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(errOut.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return refuse("couldn't read this repository's git configuration: %s", msg)
	}
	return checkConfigList(out.Bytes())
}

// checkConfigList checks `git config --list --null --show-scope` output:
// records of scope NUL key [LF value] NUL.
func checkConfigList(out []byte) error {
	fields := strings.Split(string(out), "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	if len(fields)%2 != 0 {
		return refuse("couldn't read this repository's git configuration: unexpected output from git config")
	}
	for i := 0; i < len(fields); i += 2 {
		scope := fields[i]
		key, _, _ := strings.Cut(fields[i+1], "\n")
		switch scope {
		case "command":
		case "local":
			if !keyAllowed(key) {
				return refuse("this repository's git configuration has %s, which repo-git doesn't allow", key)
			}
		default:
			return refuse("this repository's git configuration has %s (%s scope), which repo-git doesn't allow", key, scope)
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
