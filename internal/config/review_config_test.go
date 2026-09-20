package config

import (
	"strings"
	"testing"
)

// Review mode is a third AGENT_MODE, and its inputs are a contract of their
// own. These pin the two halves that matter: what review mode does NOT require
// (STEP_PROMPT — its work item is the diff), and what it will not run without
// (a baseline, because a review with no diff would report a clean bill of
// health for work it never saw).

func TestLoadReviewModeDoesNotRequireStepPrompt(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123"}`,
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load in review mode: %s", err)
	}
	if cfg.Mode != ModeReview {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeReview)
	}
	if cfg.StepPrompt != "" {
		t.Errorf("StepPrompt = %q, want empty — review mode builds its own prompt", cfg.StepPrompt)
	}
}

func TestLoadReviewModeLoadsEveryReviewInput(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_SPEC":         `{"title":"Add login"}`,
		"REVIEW_PASSES":       "security, correctness ,security",
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123","1-acme/web":"def456"}`,
		"REVIEW_ROUND":        "3",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load in review mode: %s", err)
	}
	if cfg.ReviewSpec != `{"title":"Add login"}` {
		t.Errorf("ReviewSpec = %q", cfg.ReviewSpec)
	}
	// Duplicates and whitespace are the model of a comma-separated env var,
	// not an error worth failing a review over.
	if len(cfg.ReviewPasses) != 2 || cfg.ReviewPasses[0] != "security" || cfg.ReviewPasses[1] != "correctness" {
		t.Errorf("ReviewPasses = %v, want [security correctness]", cfg.ReviewPasses)
	}
	if cfg.ReviewBaseCommits["0-acme/api"] != "abc123" || cfg.ReviewBaseCommits["1-acme/web"] != "def456" {
		t.Errorf("ReviewBaseCommits = %v", cfg.ReviewBaseCommits)
	}
	if cfg.ReviewRound != 3 {
		t.Errorf("ReviewRound = %d, want 3", cfg.ReviewRound)
	}
}

// Without a baseline there is no diff, and a review of nothing would report a
// clean bill of health for work it never saw.
func TestLoadReviewModeRequiresBaseCommits(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"absent", ""},
		{"malformed", "not json"},
		{"empty object", "{}"},
		{"empty commit", `{"0-acme/api":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, map[string]string{
				"WORK_DIR":            t.TempDir(),
				"ANTHROPIC_API_KEY":   "sk-ant-test",
				"AGENT_MODE":          ModeReview,
				"REVIEW_BASE_COMMITS": tc.value,
			})
			if _, err := Load(); err == nil {
				t.Fatal("Load succeeded with no usable baseline")
			} else if !strings.Contains(err.Error(), "REVIEW_BASE_COMMITS") {
				t.Errorf("error %q should name REVIEW_BASE_COMMITS", err)
			}
		})
	}
}

// An absent or unreadable pass list is the shipped default rather than "no
// passes": a review asked to run nothing reports nothing and looks clean.
func TestLoadReviewModeDefaultsThePassList(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123"}`,
		"REVIEW_PASSES":       "  , ",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if len(cfg.ReviewPasses) != 2 {
		t.Errorf("ReviewPasses = %v, want the default pair", cfg.ReviewPasses)
	}
	if cfg.ReviewRound != 1 {
		t.Errorf("ReviewRound = %d, want 1 when REVIEW_ROUND is absent", cfg.ReviewRound)
	}
}

// A value that is not a mode must still fail at startup — the point of the
// switch is that a typo is caught before a container does an hour of work in
// the wrong mode.
func TestLoadRejectsAnUnknownMode(t *testing.T) {
	setEnv(t, map[string]string{
		"STEP_PROMPT":       "do the thing",
		"WORK_DIR":          t.TempDir(),
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"AGENT_MODE":        "reviewish",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an unknown AGENT_MODE")
	}
	if !strings.Contains(err.Error(), "AGENT_MODE") {
		t.Errorf("error %q should name AGENT_MODE", err)
	}
}
