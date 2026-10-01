package opencode

import (
	"slices"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

func reviewEffortConfig(effort string) *config.Config {
	return &config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff",
		Model:        "anthropic/claude-sonnet-4-6",
		ReviewPasses: []string{"security"},
		ReviewEffort: effort,
	}
}

func TestBuildArgsReviewWithoutEffortIsUnchanged(t *testing.T) {
	got := (&Driver{}).BuildArgs(reviewEffortConfig(""))
	want := []string{"run", "--format", "json", "--model", "anthropic/claude-sonnet-4-6"}
	if !slices.Equal(got, want) {
		t.Errorf("review args changed with no effort set:\n got %q\nwant %q", got, want)
	}
}

func TestBuildArgsReviewPassesTheEffort(t *testing.T) {
	for _, level := range config.ReviewEffortLevels {
		t.Run(level, func(t *testing.T) {
			got := (&Driver{}).BuildArgs(reviewEffortConfig(level))
			want := []string{"run", "--format", "json", "--model", "anthropic/claude-sonnet-4-6", "--variant", level}
			if !slices.Equal(got, want) {
				t.Errorf("review args:\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestBuildArgsBatchNeverPassesAnEffort(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{StepPrompt: "hello", ReviewEffort: "high"})
	if slices.Contains(args, "--variant") {
		t.Errorf("batch args carry --variant: %q", args)
	}
}
