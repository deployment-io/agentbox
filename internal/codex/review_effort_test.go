package codex

import (
	"slices"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func reviewEffortConfig(effort string) *config.Config {
	return &config.Config{
		Mode:                 config.ModeReview,
		StepPrompt:           "the diff",
		Model:                "gpt-5",
		ReviewPasses:         []string{"security"},
		ReviewReadOnlyMounts: true,
		ReviewEffort:         effort,
	}
}

// todaysReviewArgs is the review invocation as it was before REVIEW_EFFORT
// existed, spelled out so an unset effort is pinned to it byte for byte.
func todaysReviewArgs() []string {
	return []string{
		"exec", "--json",
		"--sandbox", "danger-full-access",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"-c", "features.plugins=false",
		"-c", "check_for_update_on_startup=false",
		"-c", "analytics.enabled=false",
		"-c", "otel.exporter=none",
		"-c", "otel.metrics_exporter=none",
		"--model", "gpt-5",
	}
}

func TestBuildArgsReviewWithoutEffortIsUnchanged(t *testing.T) {
	if got, want := (&Driver{}).BuildArgs(reviewEffortConfig("")), todaysReviewArgs(); !slices.Equal(got, want) {
		t.Errorf("review args changed with no effort set:\n got %q\nwant %q", got, want)
	}
}

func TestBuildArgsReviewPassesTheEffort(t *testing.T) {
	for _, level := range config.ReviewEffortLevels {
		t.Run(level, func(t *testing.T) {
			args := (&Driver{}).BuildArgs(reviewEffortConfig(level))
			// Next to the other -c overrides, value JSON-quoted.
			want := todaysReviewArgs()
			want = slices.Insert(want, len(want)-2, "-c", `model_reasoning_effort="`+level+`"`)
			if !slices.Equal(args, want) {
				t.Errorf("review args:\n got %q\nwant %q", args, want)
			}
		})
	}
}

func TestBuildArgsBatchNeverPassesAnEffort(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{StepPrompt: "hello", ReviewEffort: "high"})
	for _, a := range args {
		if a == `model_reasoning_effort="high"` {
			t.Errorf("batch args carry an effort: %q", args)
		}
	}
}
