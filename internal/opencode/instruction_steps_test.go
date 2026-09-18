package opencode

import (
	"strings"
	"testing"
)

// The multi-repo verify contract has to be identical across drivers or the
// runner's handling stops being agent-agnostic: an opencode Step would report a
// single rollup, agentbox would have no repo to replay, and every failing
// verify would discard the Step's work — including the ones that were
// already red before the agent started.
func TestFinalMessageInstructionAsksForMultiRepoSteps(t *testing.T) {
	if !strings.Contains(finalMessageInstruction, `"steps"`) {
		t.Error("the prompt must ask for the per-repo steps array, or a multi-repo run " +
			"reports one rollup and agentbox can't tell which repo failed")
	}
	if !strings.Contains(finalMessageInstruction, `"repo"`) {
		t.Error("each step must name its repo — that field is what the baseline replay resolves")
	}
	if !strings.Contains(finalMessageInstruction, "blocks the commit and push") {
		t.Error("the prompt must say a failed verify blocks the push")
	}
}
