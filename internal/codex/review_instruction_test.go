package codex

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

// The reviewer must not be ABLE to write. codex enforces that with its own
// sandbox, so review mode asks for read-only and — critically — drops the flag
// that bypasses the sandbox entirely.
func TestBuildArgsMakesReviewReadOnly(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff and the spec",
		ReviewPasses: []string{"security"},
		MCPSocket:    "/run/agentbox/tool-rpc.sock",
	})

	if !hasPair(args, "--sandbox", "read-only") {
		t.Errorf("review mode does not request the read-only sandbox: %v", args)
	}
	for _, arg := range args {
		switch arg {
		case "--dangerously-bypass-approvals-and-sandbox":
			t.Error("review mode bypasses the sandbox, so read-only buys nothing")
		case "danger-full-access":
			t.Error("review mode still asks for full filesystem access")
		}
		if strings.HasPrefix(arg, "mcp_servers.") {
			t.Errorf("review mode wires MCP tools (%s); a reviewer needs none", arg)
		}
	}
}

// Batch mode keeps full autonomy — the Review stage must not quietly restrict
// the implementer.
func TestBuildArgsKeepsBatchModeAutonomous(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"})
	if !hasPair(args, "--sandbox", "danger-full-access") {
		t.Errorf("batch mode lost danger-full-access: %v", args)
	}
	if !strings.Contains(strings.Join(args, "\n"), "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("batch mode lost its approval bypass: %v", args)
	}
}

func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
