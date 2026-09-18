package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/verify"
)

// End-to-end over the shape that actually ships: the agent's <verify>
// trailer, through the parser, through the baseline replay, into
// result.json. The pieces are unit-tested apart; this pins that they agree
// on one payload — a two-repo run where one repo fails and the failure was
// already there.
func TestMultiStepVerifyRoundTripsWithBaseline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	workDir := tempWorkDir(t)
	apiDir, apiSHA := initRepo(t, workDir, "0-acme/api", 1) // red before the agent
	webDir, webSHA := initRepo(t, workDir, "1-acme/web", 0)

	// Exactly what a driver's parser receives at the end of a run.
	final := `Touched both repos.

<verify>{"ran":true,"passed":false,"command":"sh ./check.sh","steps":[` +
		`{"repo":"0-acme/api","command":"sh ./check.sh","passed":false,"stderr_tail":"check exit=1"},` +
		`{"repo":"1-acme/web","command":"sh ./check.sh","passed":true}` +
		`]}</verify>

<pr_title>Touch both repos</pr_title>`

	summary, vr := splitVerifyTrailer(final)
	if vr == nil {
		t.Fatal("parser dropped the <verify> trailer")
	}
	if strings.Contains(summary, "<verify>") {
		t.Error("the trailer must be stripped out of the summary")
	}
	if len(vr.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(vr.Steps))
	}
	if vr.Steps[0].Repo != "0-acme/api" || vr.Steps[0].Passed {
		t.Errorf("first step = %+v, want the failing api repo", vr.Steps[0])
	}
	if !vr.Steps[1].Passed {
		t.Errorf("second step = %+v, want it passing", vr.Steps[1])
	}

	verify.Annotate(context.Background(), vr, verify.Options{
		WorkDir:      workDir,
		StartCommits: map[string]string{apiDir: apiSHA, webDir: webSHA},
	})

	resultPath := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", resultPath)
	if err := result.Write(result.Outcome{
		Status:       result.StatusSuccess,
		VerifyResult: vr,
	}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		VerifyResult struct {
			Ran         bool `json:"ran"`
			Passed      bool `json:"passed"`
			PreExisting bool `json:"pre_existing"`
			Steps       []struct {
				Repo               string `json:"repo"`
				Passed             bool   `json:"passed"`
				BaselineRan        bool   `json:"baseline_ran"`
				BaselinePassed     bool   `json:"baseline_passed"`
				BaselineStderrTail string `json:"baseline_stderr_tail"`
			} `json:"steps"`
		} `json:"verify_result"`
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("result.json is not readable: %v\n%s", err, data)
	}

	rv := got.VerifyResult
	if !rv.Ran || rv.Passed {
		t.Errorf("rollup = ran:%v passed:%v, want the rollup preserved verbatim", rv.Ran, rv.Passed)
	}
	if len(rv.Steps) != 2 {
		t.Fatalf("result.json carries %d steps, want 2", len(rv.Steps))
	}
	failing := rv.Steps[0]
	if !failing.BaselineRan {
		t.Error("the failing step must carry baseline_ran")
	}
	if failing.BaselinePassed {
		t.Error("baseline_passed = true although the repo was already red")
	}
	if !strings.Contains(failing.BaselineStderrTail, "check exit=1") {
		t.Errorf("baseline_stderr_tail = %q, want the baseline run's own output", failing.BaselineStderrTail)
	}
	if rv.Steps[1].BaselineRan {
		t.Error("the passing step was replayed")
	}
	if !rv.PreExisting {
		t.Error("pre_existing = false although every FAILED step also fails on base")
	}
}

// The single-command payload every shipped agentbox emits today must survive
// untouched: no steps, and no pre_existing key for an older runner to trip
// over.
func TestLegacyVerifyPayloadRoundTripsUnchanged(t *testing.T) {
	_, vr := splitVerifyTrailer(`<verify>{"ran":true,"passed":false,"command":"go test ./...","stderr_tail":"boom"}</verify>`)
	if vr == nil {
		t.Fatal("parser dropped the trailer")
	}
	if len(vr.Steps) != 0 {
		t.Errorf("steps = %+v, want none", vr.Steps)
	}
	verify.Annotate(context.Background(), vr, verify.Options{WorkDir: tempWorkDir(t)})

	resultPath := filepath.Join(t.TempDir(), "result.json")
	t.Setenv("RESULT_PATH", resultPath)
	if err := result.Write(result.Outcome{Status: result.StatusSuccess, VerifyResult: vr}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pre_existing", "steps", "baseline_ran"} {
		if strings.Contains(string(data), key) {
			t.Errorf("result.json emitted %q for a legacy payload:\n%s", key, data)
		}
	}
}

func tempWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// initRepo creates a repository under workDir whose check.sh exits with the
// given code, and returns its directory plus the commit it is at.
func initRepo(t *testing.T, workDir, rel string, exitCode int) (string, string) {
	t.Helper()
	dir := filepath.Join(workDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	script := "#!/bin/sh\necho \"check exit=1\" >&2\nexit 1\n"
	if exitCode == 0 {
		script = "#!/bin/sh\nexit 0\n"
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	return dir, git("rev-parse", "HEAD")
}
