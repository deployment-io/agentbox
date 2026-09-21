package opencode

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

// opencode takes its permissions from the config file the driver writes, not
// from a flag, so that is where review mode's read-only guarantee lives. Edit
// and bash are denied; opencode's read / grep / glob tools are untouched, which
// is everything a reviewer needs.
func TestAgentConfigDeniesWritesInReviewMode(t *testing.T) {
	cfg := agentConfig("/run/agentbox/tool-rpc.sock", true)

	perms, ok := cfg["permission"].(map[string]any)
	if !ok {
		t.Fatalf("permission = %#v, want a per-tool map denying writes", cfg["permission"])
	}
	for _, tool := range []string{"edit", "bash", "webfetch"} {
		if perms[tool] != "deny" {
			t.Errorf("permission[%q] = %v, want \"deny\"", tool, perms[tool])
		}
	}
	// Nothing may be left at opencode's "ask" default: a headless run has
	// nobody to ask. Reads outside cwd stay open because the repos live
	// there.
	for _, tool := range []string{"external_directory", "doom_loop"} {
		if perms[tool] != "allow" {
			t.Errorf("permission[%q] = %v, want \"allow\" — an unanswered ask is a hang", tool, perms[tool])
		}
	}
	for tool, v := range perms {
		if v == "ask" {
			t.Errorf("permission[%q] = ask; every class must be decided for a headless reviewer", tool)
		}
	}
	if _, wired := cfg["mcp"]; wired {
		t.Error("review mode wires MCP tools; a reviewer needs none")
	}
}

// Batch mode keeps full autonomy — a headless implement run must never block
// on a permission prompt.
func TestAgentConfigKeepsBatchModeAutonomous(t *testing.T) {
	cfg := agentConfig("", false)
	if cfg["permission"] != "allow" {
		t.Errorf("permission = %v, want \"allow\" for an implement run", cfg["permission"])
	}
}
