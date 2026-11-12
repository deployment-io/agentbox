package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets a test run this binary's main as `agentbox <args>`: with
// AGENTBOX_TEST_RUN_MAIN=1 the test binary is agentbox.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTBOX_TEST_RUN_MAIN") == "1" {
		os.Args = append([]string{"agentbox"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runAgentbox(t *testing.T, work string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "AGENTBOX_TEST_RUN_MAIN=1", "WORK_DIR="+work)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// repo-git prints git's own output and nothing else: no banner, no config
// loading (no STEP_PROMPT or credentials are set here).
func TestRepoGitHasNoBanner(t *testing.T) {
	work := t.TempDir()
	repo := filepath.Join(work, "0-acme", "web")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("/usr/bin/git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com",
		"init", "-q", "-b", "main")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	commit := exec.Command("/usr/bin/git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@example.com",
		"commit", "-q", "--allow-empty", "-m", "first")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	code, out, errOut := runAgentbox(t, work, "repo-git", "0-acme/web", "log", "--format=%s")
	if code != 0 || out != "first\n" || errOut != "" {
		t.Errorf("log: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = runAgentbox(t, work, "repo-git", "0-acme/web", "fetch")
	if code != 2 || out != "" || !strings.HasPrefix(errOut, "[repo-git] ") || strings.Count(errOut, "\n") != 1 {
		t.Errorf("refusal: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
