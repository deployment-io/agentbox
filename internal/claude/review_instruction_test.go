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

// On VERIFIED read-only mounts the reviewer gets a plain Bash, so it can run
// the repository's build and tests — the independent check on the
// implementer's self-reported verify result, and what a Codex reviewer on the
// same mounts has been doing all along. What it must NOT gain is a tool that
// edits: the mounts make the tree unwritable, and the allowlist keeps Edit,
// Write and NotebookEdit off the table regardless, so neither guarantee rests
// on the other.
func TestReviewGetsUnrestrictedBashOnVerifiedReadOnlyMounts(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		Mode:                 config.ModeReview,
		StepPrompt:           "the diff and the spec",
		ReviewPasses:         []string{"security"},
		ReviewReadOnlyMounts: true,
		MCPSocket:            "/run/agentbox/tool-rpc.sock",
	})

	idx := indexOf(args, "--allowedTools")
	if idx < 0 {
		t.Fatalf("review mode has no --allowedTools allowlist: %v", args)
	}
	allowed := args[idx+1:]
	if indexOf(allowed, "Bash") < 0 {
		t.Errorf("read-only mounts did not grant a plain Bash: %v", allowed)
	}
	for _, entry := range allowed {
		if strings.HasPrefix(entry, "Bash(") {
			t.Errorf("a restricted Bash pattern (%q) survived alongside the plain Bash, which is dead weight: %v", entry, allowed)
		}
		for _, writer := range []string{"Edit", "Write", "NotebookEdit"} {
			if strings.HasPrefix(entry, writer) {
				t.Errorf("read-only mounts granted the writing tool %q", entry)
			}
		}
	}
	// The reading tools are the reason a reviewer can review at all.
	for _, reader := range []string{"Read", "Grep", "Glob"} {
		if indexOf(allowed, reader) < 0 {
			t.Errorf("the allowlist lost %q: %v", reader, allowed)
		}
	}
	// Everything else review mode withholds is unchanged by the mounts.
	if indexOf(args, "--dangerously-skip-permissions") >= 0 {
		t.Error("read-only mounts bypassed the allowlist entirely")
	}
	if indexOf(args, "--mcp-config") >= 0 {
		t.Error("read-only mounts wired MCP tools into a review")
	}
	// Still variadic, so it still has to be last.
	if idx != len(args)-1-len(allowed) {
		t.Errorf("--allowedTools is not the final flag: %v", args)
	}
}

// Without the claim — an older runner, or a probe that found a writable
// repository and withdrew it — the allowlist is byte-for-byte what it was.
// There is nothing in the kernel stopping a write, so `go build` is denied
// along with everything else not on the list.
func TestReviewKeepsTheAllowlistWithoutVerifiedReadOnlyMounts(t *testing.T) {
	d := &Driver{}
	args := d.BuildArgs(&config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the diff and the spec",
		ReviewPasses: []string{"security"},
	})

	idx := indexOf(args, "--allowedTools")
	if idx < 0 {
		t.Fatalf("review mode has no --allowedTools allowlist: %v", args)
	}
	allowed := args[idx+1:]
	if len(allowed) != len(readOnlyAllowedTools) {
		t.Fatalf("allowlist = %v, want the read-only list %v", allowed, readOnlyAllowedTools)
	}
	for i, want := range readOnlyAllowedTools {
		if allowed[i] != want {
			t.Errorf("allowlist[%d] = %q, want %q", i, allowed[i], want)
		}
	}
	if indexOf(allowed, "Bash") >= 0 {
		t.Error("an unrestricted Bash was granted with no read-only guarantee behind it")
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

// A review prompt is delivered on stdin, where no argv limit applies: -p is
// left bare so claude reads the prompt from stdin, and the trailer instruction
// still rides --append-system-prompt. An implement run is unchanged: prompt
// after -p, nothing on stdin.
func TestReviewPromptGoesOnStdin(t *testing.T) {
	d := &Driver{}
	review := &config.Config{
		Mode:         config.ModeReview,
		StepPrompt:   "the change index and the spec",
		ReviewPasses: []string{"security"},
	}
	if in := d.Stdin(review); in != review.StepPrompt {
		t.Errorf("review stdin = %q, want the prompt", in)
	}
	args := d.BuildArgs(review)
	if len(args) < 2 || args[0] != "-p" || args[1] != "--append-system-prompt" {
		t.Errorf("review args = %v, want a bare -p followed by --append-system-prompt", args[:min(len(args), 3)])
	}
	for _, arg := range args {
		if strings.Contains(arg, review.StepPrompt) {
			t.Errorf("review args carry the prompt (%q); it must travel on stdin only", arg)
		}
	}

	batch := &config.Config{Mode: config.ModeBatch, StepPrompt: "do the thing"}
	if in := d.Stdin(batch); in != "" {
		t.Errorf("batch stdin = %q, want nothing", in)
	}
	if bargs := d.BuildArgs(batch); len(bargs) < 2 || bargs[0] != "-p" || bargs[1] != "do the thing" {
		t.Errorf("batch args = %v, want the prompt right after -p", bargs[:min(len(bargs), 2)])
	}
}
