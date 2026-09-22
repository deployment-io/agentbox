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

// A pass name this image cannot run is DROPPED, not carried. Carried through,
// it reaches the prompt as a pass the agent is asked to run and the trailer
// instruction as a parameter it may report against — producing findings under
// a parameter no consumer can map. This is the newer-runner / older-image
// shape, and it has to stay visible rather than quiet.
func TestLoadReviewModeDropsUnknownPasses(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123"}`,
		"REVIEW_PASSES":       "security,performance,correctness",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	for _, p := range cfg.ReviewPasses {
		if !knownReviewPasses[p] {
			t.Errorf("ReviewPasses carries %q, which maps to no parameter this image can report", p)
		}
	}
	if len(cfg.ReviewPasses) != 2 {
		t.Errorf("ReviewPasses = %v, want the two runnable passes", cfg.ReviewPasses)
	}
}

// Every name unknown is the same as none supplied: the shipped default, not a
// review that runs nothing and looks clean.
func TestLoadReviewModeFallsBackWhenEveryPassIsUnknown(t *testing.T) {
	setEnv(t, map[string]string{
		"WORK_DIR":            t.TempDir(),
		"ANTHROPIC_API_KEY":   "sk-ant-test",
		"AGENT_MODE":          ModeReview,
		"REVIEW_BASE_COMMITS": `{"0-acme/api":"abc123"}`,
		"REVIEW_PASSES":       "performance,maintainability",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if len(cfg.ReviewPasses) != len(defaultReviewPasses) {
		t.Errorf("ReviewPasses = %v, want the default pair", cfg.ReviewPasses)
	}
}

// Every key is joined onto WORK_DIR and handed to git -C. A key that escapes
// the work dir would point the review at a repository outside the Step's
// workspace, so it is rejected rather than resolved.
func TestLoadReviewModeRejectsBaseCommitKeysOutsideTheWorkDir(t *testing.T) {
	for _, key := range []string{"../elsewhere", "/etc", "0-acme/../../etc"} {
		t.Run(key, func(t *testing.T) {
			setEnv(t, map[string]string{
				"WORK_DIR":            t.TempDir(),
				"ANTHROPIC_API_KEY":   "sk-ant-test",
				"AGENT_MODE":          ModeReview,
				"REVIEW_BASE_COMMITS": `{"` + key + `":"abc123"}`,
			})
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted the escaping key %q", key)
			} else if !strings.Contains(err.Error(), "WORK_DIR") {
				t.Errorf("error %q should say the key must sit inside WORK_DIR", err)
			}
		})
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
