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
//
// In review mode the prompt and its instruction travel on STDIN (see
// TestReviewPromptGoesOnStdin), so the contract is asserted there.
func TestBuildArgsSwapsTheInstructionInReviewMode(t *testing.T) {
	d := &Driver{}
	cfg := &config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff and the spec",
		ReviewPasses: []string{"security", "correctness"},
	}
	joined := strings.Join(d.BuildArgs(cfg), "\n") + "\n" + d.Stdin(cfg)

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

// Without REVIEW_READONLY_MOUNTS the runner has made no promise about the
// mounts, so codex's own sandbox is what keeps the reviewer from writing:
// review mode asks for read-only and — critically — drops the flag that
// bypasses the sandbox entirely. This is the older-runner path, and it stands
// even though the sandbox makes the review useless inside this container: a
// review that cannot run is safer than one that could write.
func TestBuildArgsMakesReviewReadOnlyWithoutReadOnlyMounts(t *testing.T) {
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

// With REVIEW_READONLY_MOUNTS the repositories are mounted read-only, so the
// tree is already unwritable and codex's sandbox buys nothing — while costing
// everything, because it is bubblewrap-based and every command it wraps fails
// inside agentbox's container. So the review runs with the implement run's
// flags. Everything else review mode does is unchanged: no MCP channel, and
// the prompt still on stdin.
func TestBuildArgsUsesFullAccessForReviewWithReadOnlyMounts(t *testing.T) {
	d := &Driver{}
	cfg := &config.Config{
		Mode:                 config.ModeReview,
		StepPrompt:           "the diff and the spec",
		ReviewPasses:         []string{"security"},
		ReviewReadOnlyMounts: true,
		MCPSocket:            "/run/agentbox/tool-rpc.sock",
	}
	args := d.BuildArgs(cfg)

	if !hasPair(args, "--sandbox", "danger-full-access") {
		t.Errorf("review with read-only mounts does not ask for danger-full-access: %v", args)
	}
	if hasPair(args, "--sandbox", "read-only") {
		t.Errorf("review with read-only mounts still asks for codex's bwrap sandbox: %v", args)
	}
	if !contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("review with read-only mounts did not bypass the sandbox: %v", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "mcp_servers.") {
			t.Errorf("review mode wires MCP tools (%s); a reviewer needs none", arg)
		}
		if strings.Contains(arg, cfg.StepPrompt) || strings.Contains(arg, "<review>") {
			t.Errorf("review args carry the prompt (%q); it must travel on stdin only", arg)
		}
	}
	if in := d.Stdin(cfg); !strings.HasPrefix(in, cfg.StepPrompt) || !strings.Contains(in, "<review>") {
		t.Errorf("review stdin = %q, want the prompt followed by the trailer instruction", in)
	}
	for _, unwanted := range []string{"<verify>", "<pr_title>"} {
		if strings.Contains(d.Stdin(cfg), unwanted) {
			t.Errorf("review still asks for %s — the implementer's instruction must not be appended", unwanted)
		}
	}
}

// The flag is a review-mode input; an implement run never sets it and must
// keep exactly the args it had.
func TestBuildArgsForBatchIgnoresReadOnlyMounts(t *testing.T) {
	d := &Driver{}
	plain := d.BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"})
	flagged := d.BuildArgs(&config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing", ReviewReadOnlyMounts: true})

	if strings.Join(plain, "\x00") != strings.Join(flagged, "\x00") {
		t.Errorf("batch args changed with the review flag set:\n %v\n %v", plain, flagged)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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

// A review prompt carries the whole change index and is delivered on stdin,
// where no argv limit applies; the args carry no prompt at all, so the CLI
// reads its instructions from stdin. An implement run is unchanged: prompt in
// the args, nothing on stdin.
func TestReviewPromptGoesOnStdin(t *testing.T) {
	d := &Driver{}
	review := &config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the change index and the spec",
		ReviewPasses: []string{"security"},
	}
	if in := d.Stdin(review); !strings.HasPrefix(in, review.StepPrompt) || !strings.Contains(in, "<review>") {
		t.Errorf("review stdin = %q, want the prompt followed by the trailer instruction", in)
	}
	for _, arg := range d.BuildArgs(review) {
		if strings.Contains(arg, review.StepPrompt) || strings.Contains(arg, "<review>") {
			t.Errorf("review args carry the prompt (%q); it must travel on stdin only", arg)
		}
	}

	batch := &config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"}
	if in := d.Stdin(batch); in != "" {
		t.Errorf("batch stdin = %q, want nothing", in)
	}
	if !strings.Contains(strings.Join(d.BuildArgs(batch), "\n"), "do the thing") {
		t.Error("batch args no longer carry the prompt")
	}
}
