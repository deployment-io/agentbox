package claude

import (
	"strings"
	"testing"
)

// assertStepsFormRequested pins the parts of the multi-repo verify contract
// the downstream baseline replay depends on. Each driver carries its own copy
// of finalMessageInstruction (see the Rule-of-Three note in the opencode
// driver); this is the shared check the codex and opencode packages mirror.
func assertStepsFormRequested(t *testing.T, instruction string) {
	t.Helper()
	if !strings.Contains(instruction, `"steps"`) {
		t.Error("the prompt must ask for the per-repo steps array, or a multi-repo run " +
			"reports one rollup and agentbox can't tell which repo failed")
	}
	if !strings.Contains(instruction, `"repo"`) {
		t.Error("each step must name its repo — that field is what the baseline replay resolves")
	}
	if !strings.Contains(instruction, "blocks the commit and push") {
		t.Error("the prompt must say a failed verify blocks the push; without the stakes " +
			"the agent has no reason to fix what it can first")
	}
}
