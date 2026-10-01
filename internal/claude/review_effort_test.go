package claude

import (
	"slices"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/review"
)

func reviewEffortConfig(effort string) *config.Config {
	return &config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff",
		Model:        "opus",
		MaxTurns:     "40",
		ReviewPasses: []string{"security"},
		ReviewEffort: effort,
	}
}

// todaysReviewArgs is the review invocation as it was before REVIEW_EFFORT
// existed, spelled out so an unset effort is pinned to it byte for byte.
func todaysReviewArgs(cfg *config.Config) []string {
	args := []string{
		"-p",
		"--append-system-prompt", review.Instruction(cfg),
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "40",
		"--model", "opus",
		"--allowedTools",
	}
	return append(args, reviewAllowedTools(false)...)
}

func TestBuildArgsReviewWithoutEffortIsUnchanged(t *testing.T) {
	cfg := reviewEffortConfig("")
	if got, want := (&Driver{}).BuildArgs(cfg), todaysReviewArgs(cfg); !slices.Equal(got, want) {
		t.Errorf("review args changed with no effort set:\n got %q\nwant %q", got, want)
	}
}

func TestBuildArgsReviewPassesTheEffort(t *testing.T) {
	for _, level := range config.ReviewEffortLevels {
		t.Run(level, func(t *testing.T) {
			cfg := reviewEffortConfig(level)
			args := (&Driver{}).BuildArgs(cfg)
			assertFollowedBy(t, args, "--effort", level)
			// Before --allowedTools, which is variadic and would swallow it.
			if slices.Index(args, "--effort") > slices.Index(args, "--allowedTools") {
				t.Errorf("--effort comes after --allowedTools: %q", args)
			}
			// And nothing else moved.
			without := slices.Delete(slices.Clone(args), slices.Index(args, "--effort"), slices.Index(args, "--effort")+2)
			if want := todaysReviewArgs(cfg); !slices.Equal(without, want) {
				t.Errorf("args other than --effort changed:\n got %q\nwant %q", without, want)
			}
		})
	}
}

func TestBuildArgsBatchNeverPassesAnEffort(t *testing.T) {
	args := (&Driver{}).BuildArgs(&config.Config{StepPrompt: "hello", ReviewEffort: "high"})
	if slices.Contains(args, "--effort") {
		t.Errorf("batch args carry --effort: %q", args)
	}
}
