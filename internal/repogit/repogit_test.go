package repogit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// git runs the test's own git (not repo-git) to build fixtures.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitPath, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(Env(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-11-12T10:00:00Z", "GIT_COMMITTER_DATE=2026-11-12T10:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
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

// goGitConfig is what a runner clone (go-git, local branch, scrubbed URL)
// leaves in .git/config, plus a submodule url.
const goGitConfig = `[core]
	bare = false
[remote "origin"]
	url = https://github.com/acme/web.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[branch "main"]
	remote = origin
	merge = refs/heads/main
[submodule "lib"]
	url = https://github.com/acme/lib.git
`

// newWork makes a work directory holding 0-acme/web: a base commit on main
// (also refs/remotes/origin/main) and a feature branch two commits ahead,
// checked out, with .git/config replaced by a go-git clone's.
func newWork(t *testing.T) (work, repo string) {
	t.Helper()
	work = t.TempDir()
	repo = filepath.Join(work, "0-acme", "web")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "a.txt"), "one\n")
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-q", "-m", "base commit")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	git(t, repo, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(repo, "a.txt"), "one\ntwo\n")
	git(t, repo, "commit", "-q", "-am", "second line")
	write(t, filepath.Join(repo, "b.txt"), "bee\n")
	git(t, repo, "add", "b.txt")
	git(t, repo, "commit", "-q", "-m", "add b")
	write(t, filepath.Join(repo, ".git", "config"), goGitConfig)
	return work, repo
}

func runMain(work string, args ...string) (code int, stdout, stderr string) {
	return runMainStdin(work, "", args...)
}

func runMainStdin(work, stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Main(args, work, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func assertRefused(t *testing.T, work string, want string, args ...string) {
	t.Helper()
	code, out, errOut := runMain(work, args...)
	if code != 2 {
		t.Errorf("%q: exit = %d, want 2 (stdout %q, stderr %q)", args, code, out, errOut)
	}
	if out != "" {
		t.Errorf("%q: stdout = %q, want nothing", args, out)
	}
	if !strings.HasPrefix(errOut, "[repo-git] ") || strings.Count(errOut, "\n") != 1 || !strings.HasSuffix(errOut, "\n") {
		t.Errorf("%q: stderr = %q, want one [repo-git] line", args, errOut)
	}
	if want != "" && !strings.Contains(errOut, want) {
		t.Errorf("%q: stderr = %q, want it to mention %q", args, errOut, want)
	}
}

func TestSubcommands(t *testing.T) {
	work, _ := newWork(t)
	for _, sub := range []string{"log", "diff", "show", "blame", "status"} {
		args := []string{"0-acme/web", sub}
		if sub == "blame" {
			args = append(args, "a.txt")
		}
		if code, _, errOut := runMain(work, args...); code != 0 {
			t.Errorf("%s: exit = %d, stderr %q", sub, code, errOut)
		}
	}
	for _, sub := range []string{"grep", "fetch", "push", "config", "checkout", "branch", "LOG", ""} {
		assertRefused(t, work, "isn't allowed", "0-acme/web", sub)
	}
	assertRefused(t, work, "usage: agentbox repo-git <repository directory>")
	assertRefused(t, work, "usage: agentbox repo-git <repository directory>", "0-acme/web")
}

func TestPaths(t *testing.T) {
	work, repo := newWork(t)
	outside := filepath.Dir(work)

	// The absolute form inside the work directory is the same repository.
	if code, _, errOut := runMain(work, repo, "status"); code != 0 {
		t.Errorf("absolute path inside the work directory: exit %d, %q", code, errOut)
	}
	assertRefused(t, work, "isn't inside the work directory", "../x", "log")
	assertRefused(t, work, "isn't inside the work directory", "0-acme/../../x", "log")
	assertRefused(t, work, "isn't inside the work directory", outside, "log")
	assertRefused(t, work, "isn't inside the work directory", "/etc", "log")

	// A symbolic link anywhere in the path, even one to the real repository.
	if err := os.Symlink(filepath.Join(work, "0-acme"), filepath.Join(work, "link-owner")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repo, filepath.Join(work, "0-acme", "link-repo")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, work, "symbolic link", "link-owner/web", "log")
	assertRefused(t, work, "symbolic link", "0-acme/link-repo", "log")

	// No .git, a .git file, a .git symlink to a real .git directory.
	plain := filepath.Join(work, "1-acme", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, work, "no .git directory", "1-acme/plain", "log")
	assertRefused(t, work, "", "1-acme/missing", "log")
	write(t, filepath.Join(work, "2-acme", "sub", ".git"), "gitdir: ../../0-acme/web/.git\n")
	assertRefused(t, work, "isn't a directory", "2-acme/sub", "log")
	linked := filepath.Join(work, "3-acme", "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, work, ".git is a symbolic link", "3-acme/linked", "log")
}

func TestArguments(t *testing.T) {
	work, _ := newWork(t)
	for _, bad := range [][]string{
		{"--output=x"}, {"--output", "x"}, {"--outp=x"}, {"--ext-diff"}, {"--ext-d"},
		{"--textconv"}, {"--textc"},
		{"--", "--output=x"}, {"--", "--ext-diff"}, {"--", "--textc"},
	} {
		args := append([]string{"0-acme/web", "diff"}, bad...)
		assertRefused(t, work, "isn't allowed", args...)
		if _, err := os.Stat(filepath.Join(work, "x")); err == nil {
			t.Fatalf("%q wrote a file", bad)
		}
	}
	for _, ok := range [][]string{
		{"--text"}, {"--no-ext-diff"}, {"--no-textconv"}, {"--output-indicator-new=+"},
		{"--output-indicator-old=-", "--output-indicator-context=x"},
		{"--stat", "origin/main...HEAD"}, {"origin/main", "--", "a.txt"},
	} {
		_, _, rest, err := Parse(append([]string{"0-acme/web", "diff"}, ok...), work)
		if err != nil || !slices.Equal(rest, ok) {
			t.Errorf("%q: rest = %q, err = %v; want passed through unchanged", ok, rest, err)
		}
		if code, _, errOut := runMain(work, append([]string{"0-acme/web", "diff"}, ok...)...); code != 0 {
			t.Errorf("%q: exit %d, %q", ok, code, errOut)
		}
	}
}

func TestGitArgsAndEnv(t *testing.T) {
	prefix := []string{"--no-pager", "--no-optional-locks", "-C", "/work/0-acme/web",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"-c", "core.pager=cat", "-c", "protocol.allow=never"}
	for sub, flags := range map[string][]string{
		"log":    {"--no-ext-diff", "--no-textconv"},
		"diff":   {"--no-ext-diff", "--no-textconv"},
		"show":   {"--no-ext-diff", "--no-textconv"},
		"blame":  {"--no-textconv"},
		"status": nil,
	} {
		want := append(append(append(slices.Clone(prefix), sub), flags...), "--x", "y")
		if got := GitArgs("/work/0-acme/web", sub, []string{"--x", "y"}); !slices.Equal(got, want) {
			t.Errorf("%s: argv = %q\nwant %q", sub, got, want)
		}
	}
	wantLsFiles := append(slices.Clone(prefix), "ls-files", "--stage", "-z")
	if got := LsFilesArgs("/work/0-acme/web"); !slices.Equal(got, wantLsFiles) {
		t.Errorf("ls-files argv = %q", got)
	}
	wantConfig := append(slices.Clone(prefix), "config", "--list", "--null", "--show-scope")
	if got := ConfigArgs("/work/0-acme/web"); !slices.Equal(got, wantConfig) {
		t.Errorf("config argv = %q", got)
	}
	wantEnv := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C.UTF-8", "TERM=dumb",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	if got := Env(); !slices.Equal(got, wantEnv) {
		t.Errorf("env = %q", got)
	}
}

func TestCallerEnvironmentDoesNotReachGit(t *testing.T) {
	work, _ := newWork(t)
	other := t.TempDir()
	git(t, other, "init", "-q", "-b", "main")
	git(t, other, "commit", "-q", "--allow-empty", "-m", "the wrong repository")
	trace := filepath.Join(t.TempDir(), "trace")

	t.Setenv("GIT_EXEC_PATH", t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_TRACE", trace)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.pager'='touch "+trace+"'")
	t.Setenv("LD_PRELOAD", "/nonexistent/libevil.so")

	code, out, errOut := runMain(work, "0-acme/web", "log", "--format=%s")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != "add b\nsecond line\nbase commit\n" {
		t.Errorf("log = %q, want the repository's own commits", out)
	}
	if _, err := os.Stat(trace); err == nil {
		t.Error("GIT_TRACE reached git: the trace file was written")
	}
}

func TestConfigCheck(t *testing.T) {
	work, repo := newWork(t)
	// A clean go-git config runs.
	if code, _, errOut := runMain(work, "0-acme/web", "status"); code != 0 {
		t.Fatalf("clean config: exit %d, %q", code, errOut)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(t.TempDir(), "evil.sh")
	write(t, script, "#!/bin/sh\ntouch "+marker+"\ncat\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(t.TempDir(), "hooks")
	for _, name := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		write(t, filepath.Join(hooks, name), "#!/bin/sh\ntouch "+marker+"\n")
		os.Chmod(filepath.Join(hooks, name), 0o755)
	}
	included := filepath.Join(t.TempDir(), "included")
	write(t, included, "[core]\n\tpager = "+script+"\n")
	write(t, filepath.Join(repo, ".gitattributes"), "* diff=x filter=x\n")
	// A working-tree change so diff and status have something to filter.
	write(t, filepath.Join(repo, "a.txt"), "changed\n")

	for _, c := range []struct{ key, section string }{
		{"core.fsmonitor", "[core]\n\tfsmonitor = " + script},
		{"core.hooksPath", "[core]\n\thooksPath = " + hooks},
		{"core.pager", "[core]\n\tpager = " + script},
		{"core.sshCommand", "[core]\n\tsshCommand = " + script},
		{"diff.external", "[diff]\n\texternal = " + script},
		{"diff.x.textconv", "[diff \"x\"]\n\ttextconv = " + script},
		{"diff.x.command", "[diff \"x\"]\n\tcommand = " + script},
		{"filter.x.clean", "[filter \"x\"]\n\tclean = " + script},
		{"include.path", "[include]\n\tpath = " + included},
		{"credential.helper", "[credential]\n\thelper = " + script},
		{"submodule.x.update", "[submodule \"x\"]\n\tupdate = !" + script},
	} {
		write(t, filepath.Join(repo, ".git", "config"), goGitConfig+c.section+"\n")
		for _, sub := range []string{"diff", "status", "log", "show"} {
			assertRefused(t, work, "has "+strings.ToLower(c.key), "0-acme/web", sub)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the script it names was run", c.key)
		}
	}
}

// The -c settings and flags hold even for a configuration that changed after
// the check: run git directly, without it.
func TestOverridesHoldWithoutTheCheck(t *testing.T) {
	_, repo := newWork(t)
	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(t.TempDir(), "evil.sh")
	write(t, script, "#!/bin/sh\ntouch "+marker+"\ncat\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, ".gitattributes"), "* diff=x\n")
	write(t, filepath.Join(repo, "a.txt"), "changed\n")
	write(t, filepath.Join(repo, ".git", "config"), goGitConfig+
		"[core]\n\tpager = "+script+"\n\tfsmonitor = "+script+"\n"+
		"[diff]\n\texternal = "+script+"\n[diff \"x\"]\n\ttextconv = "+script+"\n")
	for _, sub := range []string{"diff", "log", "show", "status"} {
		var out, errOut bytes.Buffer
		args := []string{}
		if sub == "log" {
			args = []string{"-p"}
		}
		if code := run(GitArgs(repo, sub, args), nil, &out, &errOut); code != 0 {
			t.Errorf("%s: exit %d, %q", sub, code, errOut.String())
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s ran the configured program", sub)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	work, repo := newWork(t)

	code, out, errOut := runMain(work, "0-acme/web", "log", "--oneline", "--format=%s", "origin/main..HEAD")
	if code != 0 || out != "add b\nsecond line\n" || errOut != "" {
		t.Errorf("log: %d %q %q", code, out, errOut)
	}
	code, out, _ = runMain(work, "0-acme/web", "diff", "--stat", "origin/main...HEAD")
	if code != 0 || !strings.Contains(out, "a.txt | 1 +") || !strings.Contains(out, "b.txt | 1 +") ||
		!strings.Contains(out, "2 files changed, 2 insertions(+)") {
		t.Errorf("diff --stat: %d %q", code, out)
	}
	code, out, _ = runMain(work, "0-acme/web", "diff", "origin/main...HEAD", "--", "a.txt")
	if code != 0 || !strings.Contains(out, "+two\n") || strings.Contains(out, "b.txt") {
		t.Errorf("diff: %d %q", code, out)
	}
	code, out, _ = runMain(work, "0-acme/web", "show", "--format=%s", "HEAD")
	if code != 0 || !strings.HasPrefix(out, "add b\n") || !strings.Contains(out, "+bee\n") {
		t.Errorf("show: %d %q", code, out)
	}
	code, out, _ = runMain(work, "0-acme/web", "blame", "--porcelain", "a.txt")
	if code != 0 || !strings.Contains(out, "summary base commit") || !strings.Contains(out, "summary second line") {
		t.Errorf("blame: %d %q", code, out)
	}
	// Git's own exit code and stderr pass through.
	code, out, errOut = runMain(work, "0-acme/web", "show", "no-such-ref")
	if code != 128 || out != "" || !strings.Contains(errOut, "no-such-ref") || strings.Contains(errOut, "[repo-git]") {
		t.Errorf("bad ref: %d %q %q", code, out, errOut)
	}

	// status writes nothing, not even a refreshed index.
	write(t, filepath.Join(repo, "a.txt"), "changed\n")
	index := filepath.Join(repo, ".git", "index")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(index, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(index)
	code, out, _ = runMain(work, "0-acme/web", "status", "--short")
	if code != 0 || out != " M a.txt\n" {
		t.Errorf("status: %d %q", code, out)
	}
	after, _ := os.ReadFile(index)
	info, _ := os.Stat(index)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(old) {
		t.Error("status changed .git/index")
	}
}

func TestKeyAllowed(t *testing.T) {
	for _, k := range []string{"core.bare", "CORE.Bare", "core.repositoryformatversion", "extensions.objectformat",
		"remote.origin.url", "Remote.Up.Stream.FETCH", "branch.main.merge", "submodule.lib.path"} {
		if !keyAllowed(k, false) {
			t.Errorf("%s refused", k)
		}
	}
	for _, k := range []string{"core.pager", "remote.url", "remote..url", "remote.origin.pushurl",
		"branch.main.rebase", "url.x.insteadof", "core", ""} {
		if keyAllowed(k, false) {
			t.Errorf("%s allowed", k)
		}
	}
	if err := checkConfigList([]byte("global\x00user.name\nme\x00"), "this repository", false); err == nil || !strings.Contains(err.Error(), "global") {
		t.Errorf("global scope: %v", err)
	}
	if err := checkConfigList([]byte("local\x00core.bare\nfalse\x00command\x00core.pager\ncat\x00"), "this repository", false); err != nil {
		t.Errorf("clean list: %v", err)
	}
}

func TestKeyAllowedSubmodule(t *testing.T) {
	if keyAllowed("core.worktree", false) {
		t.Error("core.worktree allowed in the repository's own configuration")
	}
	if !keyAllowed("core.worktree", true) || !keyAllowed("Core.WorkTree", true) {
		t.Error("core.worktree refused in a submodule's configuration")
	}
	if keyAllowed("filter.x.clean", true) {
		t.Error("filter.x.clean allowed in a submodule's configuration")
	}
}

func TestStdinReachesGit(t *testing.T) {
	work, repo := newWork(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	code, out, errOut := runMainStdin(work, "HEAD^!\n", "0-acme/web", "log", "--stdin", "--format=%H", "-3")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != head+"\n" {
		t.Errorf("log --stdin = %q, want only %s", out, head)
	}
}

func TestRefusalIsOneLine(t *testing.T) {
	work, _ := newWork(t)
	for _, args := range [][]string{
		{"missing\n0-acme/web", "log"},
		{"0-acme/web", "log", "--output=x\ny"},
		{"0-acme/web", "grep\nlog"},
	} {
		code, _, errOut := runMain(work, args...)
		if code != 2 || !strings.HasPrefix(errOut, "[repo-git] ") || strings.Count(errOut, "\n") != 1 || !strings.HasSuffix(errOut, "\n") {
			t.Errorf("%q: exit %d, stderr %q, want one [repo-git] line", args, code, errOut)
		}
	}
}

// newWorkWithSubmodule adds a populated submodule "lib" to newWork's
// repository, with its git directory under .git/modules/lib as git and
// go-git lay it out, and returns the submodule's git directory.
func newWorkWithSubmodule(t *testing.T) (work, repo, subGitDir string) {
	t.Helper()
	work, repo = newWork(t)
	src := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q", "-b", "main")
	write(t, filepath.Join(src, "lib.txt"), "lib\n")
	git(t, src, "add", "lib.txt")
	git(t, src, "commit", "-q", "-m", "lib")
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "lib")
	git(t, repo, "commit", "-q", "-m", "add lib")
	// Back to a runner clone's parent config, plus the submodule's entries.
	write(t, filepath.Join(repo, ".git", "config"), goGitConfig+"\tactive = true\n")
	subGitDir = filepath.Join(repo, ".git", "modules", "lib")
	write(t, filepath.Join(subGitDir, "config"), `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
	worktree = ../../../lib
[remote "origin"]
	url = https://github.com/acme/lib.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[branch "main"]
	remote = origin
	merge = refs/heads/main
`)
	return work, repo, subGitDir
}

func TestSubmoduleConfigCheck(t *testing.T) {
	work, repo, subGitDir := newWorkWithSubmodule(t)
	if code, out, errOut := runMain(work, "0-acme/web", "status", "--short"); code != 0 || errOut != "" {
		t.Fatalf("clean submodule: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// A clean filter in the submodule's configuration, and a modified file
	// it applies to: git status would run it in the submodule's child git.
	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(t.TempDir(), "evil.sh")
	write(t, script, "#!/bin/sh\ntouch "+marker+"\ncat\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	clean, _ := os.ReadFile(filepath.Join(subGitDir, "config"))
	write(t, filepath.Join(subGitDir, "config"), string(clean)+"[filter \"x\"]\n\tclean = "+script+"\n")
	write(t, filepath.Join(subGitDir, "info", "attributes"), "*.txt filter=x\n")
	write(t, filepath.Join(repo, "lib", "lib.txt"), "changed\n")
	for _, args := range [][]string{
		{"0-acme/web", "status"},
		{"0-acme/web", "diff"},
		{"0-acme/web", "diff", "--submodule=diff"},
	} {
		assertRefused(t, work, "submodule lib's git configuration has filter.x.clean", args...)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the submodule's filter ran")
	}

	// The repository's own core.worktree stays refused.
	write(t, filepath.Join(subGitDir, "config"), string(clean))
	write(t, filepath.Join(repo, ".git", "config"), goGitConfig+"[core]\n\tworktree = /tmp\n")
	assertRefused(t, work, "this repository's git configuration has core.worktree", "0-acme/web", "status")
}

func TestSubmoduleSymlinkRefused(t *testing.T) {
	work, repo, _ := newWorkWithSubmodule(t)
	// Move the submodule's directory away and leave a symbolic link to it.
	moved := filepath.Join(repo, "lib-real")
	if err := os.Rename(filepath.Join(repo, "lib"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(repo, "lib")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, work, "is a symbolic link", "0-acme/web", "status")
}

func TestUnpopulatedSubmoduleRuns(t *testing.T) {
	work, repo, _ := newWorkWithSubmodule(t)
	if err := os.RemoveAll(filepath.Join(repo, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runMain(work, "0-acme/web", "log", "--format=%s", "-1"); code != 0 || out != "add lib\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestUnpopulatedSubmoduleConfigCheck(t *testing.T) {
	work, repo, subGitDir := newWorkWithSubmodule(t)
	// A second commit moving the submodule, so a diff has a submodule
	// change to show, then the working tree removed: only
	// .git/modules/lib is left.
	git(t, filepath.Join(repo, "lib"), "commit", "-q", "--allow-empty", "-m", "lib 2")
	git(t, repo, "commit", "-q", "-am", "bump lib")
	if err := os.RemoveAll(filepath.Join(repo, "lib")); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runMain(work, "0-acme/web", "log", "-p", "--submodule=diff", "-1"); code != 0 || errOut != "" {
		t.Fatalf("clean submodule repository: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	marker := filepath.Join(t.TempDir(), "marker")
	script := filepath.Join(t.TempDir(), "evil.sh")
	write(t, script, "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	clean, _ := os.ReadFile(filepath.Join(subGitDir, "config"))
	write(t, filepath.Join(subGitDir, "config"), string(clean)+"[diff]\n\texternal = "+script+"\n")
	for _, args := range [][]string{
		{"0-acme/web", "log", "-p", "--submodule=diff", "-1"},
		{"0-acme/web", "diff", "--submodule=diff", "HEAD~1", "HEAD"},
		{"0-acme/web", "status"},
	} {
		assertRefused(t, work, "submodule repository .git/modules/lib's git configuration has diff.external", args...)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the submodule repository's external diff ran")
	}

	// A module name with a slash is a nested directory under .git/modules.
	write(t, filepath.Join(subGitDir, "config"), string(clean))
	nested := filepath.Join(repo, ".git", "modules", "vendor", "x")
	git(t, t.TempDir(), "init", "-q", "--bare", nested)
	write(t, filepath.Join(nested, "config"), "[core]\n\tbare = true\n[filter \"x\"]\n\tclean = "+script+"\n")
	assertRefused(t, work, "submodule repository .git/modules/vendor/x's git configuration has filter.x.clean", "0-acme/web", "status")

	// A symbolic link under .git/modules is refused.
	if err := os.RemoveAll(filepath.Join(repo, ".git", "modules", "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(subGitDir, filepath.Join(repo, ".git", "modules", "vendor")); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, work, "is a symbolic link", "0-acme/web", "status")
}
