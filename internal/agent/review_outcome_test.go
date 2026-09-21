package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/review"
)

const rawTrailer = "Here is what I found.\n\n<review>\n" +
	`{"findings":[{"key":"sec-1","parameter":"security","severity":"high",` +
	`"location":"0-acme/api/handler.go:41","what":"no auth check","why":"anyone can read"}],` +
	`"coverage":[{"parameter":"security","state":"checked"}]}` +
	"\n</review>\n"

// The <review> block is machine payload. changes_summary is rendered verbatim
// into a pull-request body, so a block that survives there shows a reviewer raw
// JSON where the review's prose belongs — and the Error string is built from
// the agent's own final message on several failure paths, so it can carry the
// block too.
//
// This used to hold only on the clean-exit path: a review killed by SIGTERM, by
// the no-activity watchdog or by the turn cap returned straight out of the
// select with no lifting at all.
func TestLiftReviewStripsTheBlockOnEveryOutcome(t *testing.T) {
	plan := review.Plan{Coverage: review.BuildCoverage(
		[]string{review.PassSecurity, review.PassCorrectness}, nil, false)}

	for _, status := range []result.Status{
		result.StatusSuccess,
		result.StatusFailure,
		result.StatusCancelled,
		result.StatusTimeout,
	} {
		t.Run(string(status), func(t *testing.T) {
			oc := result.Outcome{
				Status:         status,
				ChangesSummary: rawTrailer,
				Error:          "claude reported error: " + rawTrailer,
				PRTitle:        "a title a reviewer has no business writing",
				FilesChanged:   []string{"0-acme/api/handler.go"},
			}
			liftReview(&oc, plan)

			for name, field := range map[string]string{
				"changes_summary": oc.ChangesSummary,
				"error":           oc.Error,
			} {
				if strings.Contains(field, "<review>") || strings.Contains(field, "\"findings\"") {
					t.Errorf("%s still carries the raw block: %q", name, field)
				}
			}
			if !strings.Contains(oc.ChangesSummary, "Here is what I found.") {
				t.Errorf("the prose was lost along with the block: %q", oc.ChangesSummary)
			}
			if oc.ReviewResult == nil {
				t.Fatal("review_result is absent; every review-mode outcome must carry one")
			}
			if len(oc.ReviewResult.Findings) != 1 {
				t.Errorf("findings = %v, want the one the trailer carried", oc.ReviewResult.Findings)
			}
			if oc.PRTitle != "" || oc.FilesChanged != nil {
				t.Error("the implementer's fields survived a review that changed nothing")
			}
		})
	}
}

// A round that did not finish must not claim it checked anything. The planned
// coverage says a pass RAN; once the run is killed that is no longer a claim
// agentbox can stand behind.
func TestLiftReviewDowngradesCoverageOnAnUnfinishedRound(t *testing.T) {
	plan := review.Plan{Coverage: review.BuildCoverage([]string{review.PassSecurity}, nil, false)}

	oc := result.Outcome{Status: result.StatusTimeout, ChangesSummary: rawTrailer}
	liftReview(&oc, plan)

	for _, c := range oc.ReviewResult.Coverage {
		if c.State == "checked" {
			t.Errorf("parameter %q reports checked after a timed-out round", c.Parameter)
		}
		if c.Reason == "" {
			t.Errorf("parameter %q reports no reason for not being checked", c.Parameter)
		}
	}

	// And on a clean exit the planned record survives untouched.
	ok := result.Outcome{Status: result.StatusSuccess, ChangesSummary: rawTrailer}
	liftReview(&ok, plan)
	if !hasState(ok.ReviewResult.Coverage, "security", "checked") {
		t.Errorf("a completed round lost its coverage: %+v", ok.ReviewResult.Coverage)
	}
}

// A diff that could not be computed is a FAILED review, never a clean one. An
// unknown base commit used to yield an empty diff, which the passes reviewed
// and reported clean — the review equivalent of a suite that passes because it
// ran no tests.
func TestRunFailsTheReviewWhenTheDiffCannotBeComputed(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "0-acme", "api"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "result.json"))
	cfg := &config.Config{
		Mode:              config.ModeReview,
		WorkDir:           workDir,
		AgentType:         "claude-code",
		ReviewPasses:      []string{review.PassSecurity},
		ReviewBaseCommits: map[string]string{"0-acme/api": "deadbeef"},
		ReviewRound:       1,
	}
	oc := Run(context.Background(), cfg, &recordingDriver{})

	if oc.Status != result.StatusFailure {
		t.Errorf("Status = %q, want failure — nothing was examined", oc.Status)
	}
	if oc.ReviewResult == nil {
		t.Fatal("review_result is absent; the record of what was NOT checked is the whole point")
	}
	if len(oc.ReviewResult.Findings) != 0 {
		t.Errorf("findings = %v, want none", oc.ReviewResult.Findings)
	}
	for _, c := range oc.ReviewResult.Coverage {
		if c.State != "not checked" {
			t.Errorf("parameter %q = %q, want not checked", c.Parameter, c.State)
		}
	}
	if !strings.Contains(oc.Error, "0-acme/api") {
		t.Errorf("Error does not name the repository that failed: %q", oc.Error)
	}
}

// The drivers derive the trailer instruction's parameter list from
// cfg.ReviewPasses. If the cost gate stands a pass down and the config still
// names it, the agent is invited to report findings for a pass that never ran.
func TestRunNarrowsReviewPassesToTheGatedListBeforeBuildArgs(t *testing.T) {
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "0-acme", "api")
	base := initTestRepo(t, repo)
	// A lockfile-only change: security runs, correctness is stood down.
	writeFile(t, filepath.Join(repo, "go.sum"), "example.com/x v1.0.0 h1:abc=\n")

	t.Setenv("RESULT_PATH", filepath.Join(t.TempDir(), "result.json"))
	driver := &recordingDriver{}
	cfg := &config.Config{
		Mode:              config.ModeReview,
		WorkDir:           workDir,
		AgentType:         "claude-code",
		ReviewPasses:      []string{review.PassSecurity, review.PassCorrectness},
		ReviewBaseCommits: map[string]string{"0-acme/api": base},
		ReviewRound:       1,
	}
	Run(context.Background(), cfg, driver)

	if !driver.built {
		t.Fatal("BuildArgs was never called")
	}
	if got := strings.Join(driver.passesAtBuild, ","); got != review.PassSecurity {
		t.Errorf("ReviewPasses at BuildArgs = %q, want only %q — the gate stood correctness down",
			got, review.PassSecurity)
	}
}

func hasState(coverage []review.Coverage, parameter, state string) bool {
	for _, c := range coverage {
		if c.Parameter == parameter && c.State == state {
			return true
		}
	}
	return false
}

// recordingDriver captures cfg.ReviewPasses at the moment BuildArgs is called,
// then runs `true` so Run completes without a real agent.
type recordingDriver struct {
	built         bool
	passesAtBuild []string
}

func (d *recordingDriver) Ensure(context.Context) error { return nil }
func (d *recordingDriver) Binary() string               { return "true" }
func (d *recordingDriver) BuildArgs(cfg *config.Config) []string {
	d.built = true
	d.passesAtBuild = append([]string{}, cfg.ReviewPasses...)
	return nil
}
func (d *recordingDriver) DetectVersion() string         { return "test" }
func (d *recordingDriver) NewOutputParser() OutputParser { return &fakeParser{} }
func (d *recordingDriver) Capabilities() Capabilities    { return Capabilities{} }
func (d *recordingDriver) AllowedHosts() []string        { return nil }
func (d *recordingDriver) NewLogFormatter(sink io.Writer) io.WriteCloser {
	return passthroughWriteCloser{sink}
}

func initTestRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	writeFile(t, filepath.Join(dir, "README.md"), "# base\n")
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
