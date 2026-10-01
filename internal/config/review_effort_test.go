package config

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func reviewEnvWithEffort(t *testing.T, effort string) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123"}`,
		"REVIEW_EFFORT":       effort,
	})
}

func TestLoadReviewModeAcceptsEveryEffortLevel(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		{"HIGH", "high"},
		{"  XHigh \n", "xhigh"},
		{" Max", "max"},
		{"", ""},
	} {
		t.Run("REVIEW_EFFORT="+tc.raw, func(t *testing.T) {
			reviewEnvWithEffort(t, tc.raw)
			var cfg *Config
			var err error
			stderr := captureStderr(t, func() { cfg, err = Load() })
			if err != nil {
				t.Fatalf("Load: %s", err)
			}
			if cfg.ReviewEffort != tc.want {
				t.Errorf("ReviewEffort = %q, want %q", cfg.ReviewEffort, tc.want)
			}
			if strings.Contains(stderr, "REVIEW_EFFORT") {
				t.Errorf("a valid or empty value warned: %q", stderr)
			}
		})
	}
}

// Codex sends an unknown effort straight to the API, which fails the run, so
// anything outside the five levels is dropped — with a warning, and without
// failing the review.
func TestLoadReviewModeRejectsAnUnknownEffort(t *testing.T) {
	reviewEnvWithEffort(t, " Bogus ")
	var cfg *Config
	var err error
	stderr := captureStderr(t, func() { cfg, err = Load() })
	if err != nil {
		t.Fatalf("Load: %s — an unknown effort must not fail the review", err)
	}
	if cfg.ReviewEffort != "" {
		t.Errorf("ReviewEffort = %q, want empty", cfg.ReviewEffort)
	}
	want := "warning: REVIEW_EFFORT=bogus is not one of low, medium, high, xhigh, max; using the model's default effort\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestLoadIgnoresReviewEffortOutsideReviewMode(t *testing.T) {
	for _, mode := range []string{ModeBatch, ModeInteractive} {
		t.Run(mode, func(t *testing.T) {
			setEnv(t, map[string]string{
				"WORK_DIR":          t.TempDir(),
				"ANTHROPIC_API_KEY": "sk-ant-test",
				"AGENT_MODE":        mode,
				"STEP_PROMPT":       "do the thing",
				"REVIEW_EFFORT":     "bogus",
			})
			var cfg *Config
			var err error
			stderr := captureStderr(t, func() { cfg, err = Load() })
			if err != nil {
				t.Fatalf("Load: %s", err)
			}
			if cfg.ReviewEffort != "" {
				t.Errorf("ReviewEffort = %q in %s mode, want empty", cfg.ReviewEffort, mode)
			}
			if strings.Contains(stderr, "REVIEW_EFFORT") {
				t.Errorf("REVIEW_EFFORT was read in %s mode: %q", mode, stderr)
			}
		})
	}
}
