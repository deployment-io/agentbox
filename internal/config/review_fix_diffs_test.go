package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// REVIEW_FIX_DIFFS points a re-check at the last fix run's own diff. It is a
// hint about where to start, so every way it can be wrong is ignored with a
// warning rather than failing the review.

func fixDiffsEnv(workDir, value, mode string) map[string]string {
	env := map[string]string{
		"WORK_DIR":          workDir,
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        mode,
		"REVIEW_FIX_DIFFS":  value,
	}
	if mode == ModeReview {
		env["REVIEW_BASE_COMMITS"] = `{"0-acme/api":"abc123","1-acme/web":"def456"}`
	} else {
		env["STEP_PROMPT"] = "do the thing"
	}
	return env
}

func writeFixDiff(t *testing.T, workDir, name string) string {
	t.Helper()
	dir := filepath.Join(workDir, ".review-fix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("diff --git a/x b/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadReviewModeParsesFixDiffs(t *testing.T) {
	work := t.TempDir()
	api := writeFixDiff(t, work, "0-acme__api.diff")
	web := writeFixDiff(t, work, "1-acme__web.diff")
	setEnv(t, fixDiffsEnv(work, `{"0-acme/api":"`+api+`","1-acme/web":"`+web+`"}`, ModeReview))

	var cfg *Config
	var err error
	stderr := captureStderr(t, func() { cfg, err = Load() })
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if len(cfg.ReviewFixDiffs) != 2 || cfg.ReviewFixDiffs["0-acme/api"] != api || cfg.ReviewFixDiffs["1-acme/web"] != web {
		t.Errorf("ReviewFixDiffs = %v", cfg.ReviewFixDiffs)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}

func TestLoadReviewModeIgnoresMalformedFixDiffs(t *testing.T) {
	setEnv(t, fixDiffsEnv(t.TempDir(), `{"0-acme/api":`, ModeReview))

	var cfg *Config
	var err error
	stderr := captureStderr(t, func() { cfg, err = Load() })
	if err != nil {
		t.Fatalf("Load must not fail on a malformed REVIEW_FIX_DIFFS: %s", err)
	}
	if len(cfg.ReviewFixDiffs) != 0 {
		t.Errorf("ReviewFixDiffs = %v, want empty", cfg.ReviewFixDiffs)
	}
	if !strings.Contains(stderr, "warning: ignoring malformed REVIEW_FIX_DIFFS") {
		t.Errorf("stderr = %q, want the malformed warning", stderr)
	}
}

func TestLoadReviewModeDropsFixDiffsOutsideTheWorkDirOrMissing(t *testing.T) {
	work := t.TempDir()
	good := writeFixDiff(t, work, "0-acme__api.diff")
	outside := filepath.Join(t.TempDir(), "outside.diff")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(work, ".review-fix", "missing.diff")
	escape := work + "/../" + filepath.Base(filepath.Dir(outside)) + "/outside.diff"
	setEnv(t, fixDiffsEnv(work, `{"0-acme/api":"`+good+`","1-acme/web":"`+outside+`","2-acme/x":"`+missing+`","3-acme/y":"`+escape+`","4-acme/z":"`+work+`"}`, ModeReview))

	var cfg *Config
	var err error
	stderr := captureStderr(t, func() { cfg, err = Load() })
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if len(cfg.ReviewFixDiffs) != 1 || cfg.ReviewFixDiffs["0-acme/api"] != good {
		t.Errorf("ReviewFixDiffs = %v, want only the entry inside WORK_DIR", cfg.ReviewFixDiffs)
	}
	for _, dir := range []string{"1-acme/web", "2-acme/x", "3-acme/y", "4-acme/z"} {
		if !strings.Contains(stderr, `warning: ignoring REVIEW_FIX_DIFFS entry "`+dir+`"`) {
			t.Errorf("stderr has no warning for %s:\n%s", dir, stderr)
		}
	}
}

func TestLoadIgnoresFixDiffsOutsideReviewMode(t *testing.T) {
	for _, mode := range []string{ModeBatch, ModeInteractive} {
		t.Run(mode, func(t *testing.T) {
			work := t.TempDir()
			path := writeFixDiff(t, work, "0-acme__api.diff")
			setEnv(t, fixDiffsEnv(work, `{"0-acme/api":"`+path+`"}`, mode))
			var cfg *Config
			var err error
			stderr := captureStderr(t, func() { cfg, err = Load() })
			if err != nil {
				t.Fatalf("Load: %s", err)
			}
			if len(cfg.ReviewFixDiffs) != 0 {
				t.Errorf("ReviewFixDiffs = %v in %s mode, want empty", cfg.ReviewFixDiffs, mode)
			}
			if strings.Contains(stderr, "REVIEW_FIX_DIFFS") {
				t.Errorf("stderr mentions REVIEW_FIX_DIFFS in %s mode: %q", mode, stderr)
			}
		})
	}
}

func TestLoadReviewModeDropsFixDiffThatIsAFIFOWithoutBlocking(t *testing.T) {
	work := t.TempDir()
	good := writeFixDiff(t, work, "0-acme__api.diff")
	fifo := filepath.Join(work, ".review-fix", "1-acme__web.diff")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %s", err)
	}
	setEnv(t, fixDiffsEnv(work, `{"0-acme/api":"`+good+`","1-acme/web":"`+fifo+`"}`, ModeReview))

	type result struct {
		cfg    *Config
		err    error
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.stderr = captureStderr(t, func() { r.cfg, r.err = Load() })
		done <- r
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Load blocked on a FIFO in REVIEW_FIX_DIFFS")
	}
	if r.err != nil {
		t.Fatalf("Load: %s", r.err)
	}
	if len(r.cfg.ReviewFixDiffs) != 1 || r.cfg.ReviewFixDiffs["0-acme/api"] != good {
		t.Errorf("ReviewFixDiffs = %v, want only the regular file", r.cfg.ReviewFixDiffs)
	}
	if !strings.Contains(r.stderr, `warning: ignoring REVIEW_FIX_DIFFS entry "1-acme/web"`) {
		t.Errorf("stderr has no warning for the FIFO:\n%s", r.stderr)
	}
}
