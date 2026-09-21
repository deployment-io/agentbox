package claude

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// A review run is held to a different contract from an implement run: it is
// asked for a <review> trailer and is NOT asked for <verify> or <pr_title>.
// Asking for those would invite a review to report a build it never ran and a
// PR title for a change it did not write.
func TestBuildArgsSwapsTheInstructionInReviewMode(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff and the spec",
		ReviewPasses: []string{"security", "correctness"},
	})
	joined := strings.Join(args, "\n")

	if !strings.Contains(joined, "<review>") {
		t.Error("review mode did not ask for a <review> trailer")
	}
	for _, unwanted := range []string{"<verify>", "<pr_title>"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("review mode still asks for %s — the implementer's instruction must not be appended", unwanted)
		}
	}
}

// And the implementer's prompt is untouched in batch mode — the Review stage
// must not change what an ordinary Step asks for.
func TestBuildArgsKeepsTheImplementerContractInBatchMode(t *testing.T) {
	d := &Driver{}
	joined := strings.Join(d.BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"}), "\n")

	for _, wanted := range []string{"<verify>", "<pr_title>"} {
		if !strings.Contains(joined, wanted) {
			t.Errorf("batch mode no longer asks for %s", wanted)
		}
	}
	if strings.Contains(joined, "<review>") {
		t.Error("batch mode asks for a <review> trailer")
	}
}

// The reviewer must not be ABLE to write, not merely be asked not to. A prompt
// is a request; the allowlist is the guarantee. Without it a reviewer that
// decides to "just fix" what it found produces a diff nobody authorised, inside
// the one stage whose job is to judge the diff it was handed.
func TestBuildArgsMakesReviewReadOnly(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff and the spec",
		ReviewPasses: []string{"security"},
		MCPSocket:    "/run/agentbox/tool-rpc.sock",
	})

	for _, arg := range args {
		if arg == "--dangerously-skip-permissions" {
			t.Error("review mode passes --dangerously-skip-permissions, which bypasses the allowlist entirely")
		}
		if arg == "--mcp-config" {
			t.Error("review mode wires MCP tools; a reviewer needs none")
		}
	}

	idx := indexOf(args, "--allowedTools")
	if idx < 0 {
		t.Fatalf("review mode has no --allowedTools allowlist: %v", args)
	}
	// Variadic: --allowedTools consumes every following token, so it has to
	// be last or it swallows a flag.
	if idx != len(args)-1-len(readOnlyAllowedTools) {
		t.Errorf("--allowedTools is not the final flag; it would consume the arguments after it: %v", args)
	}
	for _, entry := range args[idx+1:] {
		if strings.HasPrefix(entry, "Write") || strings.HasPrefix(entry, "Edit") {
			t.Errorf("allowlist entry %q can modify the tree", entry)
		}
	}
}

// Batch mode keeps full autonomy — the Review stage must not quietly restrict
// the implementer.
func TestBuildArgsKeepsBatchModeAutonomous(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"})
	if indexOf(args, "--dangerously-skip-permissions") < 0 {
		t.Errorf("batch mode lost --dangerously-skip-permissions: %v", args)
	}
	if indexOf(args, "--allowedTools") >= 0 {
		t.Errorf("batch mode gained a read-only allowlist: %v", args)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
