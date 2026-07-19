package opencode

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestParser_HappyPath drives a representative `opencode run --format json`
// stream and pins the full ParsedState mapping.
//
// VERIFY AGAINST REAL OUTPUT: the event/part shapes encoded here are the
// parser's ASSUMED schema (see parser.go). When a captured real run differs,
// update this stream first, then the structs — this test is the executable spec.
func TestParser_HappyPath(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"step_finish","part":{"tokens":{"input":1000,"output":50,"cache":{"read":200,"write":20}},"cost":0.01,"modelID":"anthropic/claude-sonnet-4-6"}}`,
		`{"type":"tool_use","part":{"tool":"edit","state":{"input":{"filePath":"main.go"}}}}`,
		`{"type":"tool_use","part":{"tool":"write","state":{"input":{"filePath":"README.md"}}}}`,
		// read + grep must NOT land in files_changed (they only inspect) — this is
		// the over-reporting/blowup guard: on a real repo the agent reads/searches
		// far more files than it edits.
		`{"type":"tool_use","part":{"tool":"read","state":{"input":{"filePath":"only_inspected.go"}}}}`,
		`{"type":"tool_use","part":{"tool":"grep","state":{"input":{"path":"/work/src"}}}}`,
		`{"type":"text","part":{"text":"Did the thing.\n\n<verify>{\"ran\":true,\"passed\":true,\"command\":\"go build ./...\"}</verify>\n\n<pr_title>Add the thing</pr_title>","modelID":"anthropic/claude-sonnet-4-6"}}`,
		`{"type":"step_finish","part":{"tokens":{"input":2000,"output":80,"cache":{"read":0,"write":0}},"cost":0.02}}`,
	}, "\n")

	p := newJSONLParser()
	p.Consume(strings.NewReader(stream))
	st := p.State()

	if st.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("Model = %q, want anthropic/claude-sonnet-4-6", st.Model)
	}
	if st.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (one per step_finish)", st.Turns)
	}
	if st.TokenUsage.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", st.TokenUsage.InputTokens)
	}
	if st.TokenUsage.OutputTokens != 130 {
		t.Errorf("OutputTokens = %d, want 130", st.TokenUsage.OutputTokens)
	}
	if st.TokenUsage.CacheReadTokens != 200 {
		t.Errorf("CacheReadTokens = %d, want 200", st.TokenUsage.CacheReadTokens)
	}
	if st.TokenUsage.CacheCreationTokens != 20 {
		t.Errorf("CacheCreationTokens = %d, want 20", st.TokenUsage.CacheCreationTokens)
	}
	if st.CostUSD == nil || math.Abs(*st.CostUSD-0.03) > 1e-9 {
		t.Errorf("CostUSD = %v, want 0.03", st.CostUSD)
	}
	if st.ChangesSummary != "Did the thing." {
		t.Errorf("ChangesSummary = %q, want %q", st.ChangesSummary, "Did the thing.")
	}
	if st.PRTitle != "Add the thing" {
		t.Errorf("PRTitle = %q, want %q", st.PRTitle, "Add the thing")
	}
	if st.VerifyResult == nil || !st.VerifyResult.Ran || !st.VerifyResult.Passed {
		t.Errorf("VerifyResult = %+v, want ran+passed", st.VerifyResult)
	}
	if want := []string{"README.md", "main.go"}; !slices.Equal(st.FilesChanged, want) {
		t.Errorf("FilesChanged = %v, want %v", st.FilesChanged, want)
	}
	if st.IsError {
		t.Error("IsError should be false on success")
	}
}

// TestParser_ApplyPatchFilesTracked verifies apply_patch (which has no filePath
// arg) contributes its target files to files_changed, parsed from the patch body.
func TestParser_ApplyPatchFilesTracked(t *testing.T) {
	patch := "*** Begin Patch\n" +
		"*** Add File: pkg/new.go\n+package pkg\n" +
		"*** Update File: pkg/old.go\n@@\n-a\n+b\n" +
		"*** Delete File: pkg/gone.go\n" +
		"*** End Patch"
	line := `{"type":"tool_use","part":{"tool":"apply_patch","state":{"input":{"patchText":` + strconv.Quote(patch) + `}}}}`
	p := newJSONLParser()
	p.processLine([]byte(line))
	if want := []string{"pkg/gone.go", "pkg/new.go", "pkg/old.go"}; !slices.Equal(p.State().FilesChanged, want) {
		t.Errorf("FilesChanged = %v, want %v", p.State().FilesChanged, want)
	}
}

// TestParser_NonMutatingToolsNotTracked pins the over-reporting guard directly:
// read/grep/glob/bash never contribute to files_changed even when they carry a
// path, so the list can't balloon with everything the agent inspected.
func TestParser_NonMutatingToolsNotTracked(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"tool_use","part":{"tool":"read","state":{"input":{"filePath":"a.go"}}}}`,
		`{"type":"tool_use","part":{"tool":"grep","state":{"input":{"path":"/work"}}}}`,
		`{"type":"tool_use","part":{"tool":"glob","state":{"input":{"path":"/work/src"}}}}`,
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{}}}}`,
	}, "\n")
	p := newJSONLParser()
	p.Consume(strings.NewReader(stream))
	if got := p.State().FilesChanged; len(got) != 0 {
		t.Errorf("FilesChanged = %v, want empty (non-mutating tools must not be tracked)", got)
	}
}

// TestParser_IncrementalTurnsVisibleMidStream pins the contract the agentbox
// limit watcher depends on: turns and usage are observable via State() BEFORE
// the stream ends, so MAX_TURNS / TOKEN_BUDGET can be enforced for opencode
// (which has no native cap), exactly like codex.
func TestParser_IncrementalTurnsVisibleMidStream(t *testing.T) {
	p := newJSONLParser()
	if got := p.State().Turns; got != 0 {
		t.Fatalf("Turns = %d before any event, want 0", got)
	}
	p.processLine([]byte(`{"type":"step_finish","part":{"tokens":{"input":10,"output":5}}}`))
	if got := p.State().Turns; got != 1 {
		t.Errorf("Turns = %d after 1 step_finish, want 1", got)
	}
	p.processLine([]byte(`{"type":"step_finish","part":{"tokens":{"input":10,"output":5}}}`))
	if got := p.State().Turns; got != 2 {
		t.Errorf("Turns = %d after 2 step_finish, want 2", got)
	}
	if got := p.State().TokenUsage.OutputTokens; got != 10 {
		t.Errorf("OutputTokens = %d, want 10", got)
	}
}

// TestParser_NoCostStaysNil verifies the pointer-distinct cost semantics: when
// opencode reports no cost, CostUSD stays nil (not a misleading $0.00).
func TestParser_NoCostStaysNil(t *testing.T) {
	p := newJSONLParser()
	p.Consume(strings.NewReader(`{"type":"step_finish","part":{"tokens":{"input":10,"output":5}}}`))
	if st := p.State(); st.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil when no cost reported", *st.CostUSD)
	}
}

func TestParser_Error(t *testing.T) {
	p := newJSONLParser()
	p.Consume(strings.NewReader(`{"type":"error","error":{"message":"something broke"}}`))
	st := p.State()
	if !st.IsError {
		t.Error("IsError should be true")
	}
	if st.FailureReason == "" {
		t.Error("FailureReason should carry the error message")
	}
}

func TestParser_AuthFailureClassified(t *testing.T) {
	p := newJSONLParser()
	p.Consume(strings.NewReader(`{"type":"error","error":{"message":"401 invalid api key"}}`))
	if !p.State().IsAuthFailure {
		t.Error("IsAuthFailure should be true for an api-key/401 error")
	}
}

func TestParser_MalformedLinesSkipped(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"type":"step_finish","part":{"tokens":{"input":5,"output":1}}}`,
		`{"type":`, // truncated json
		`{"type":"text","part":{"text":"ok"}}`,
	}, "\n")
	p := newJSONLParser()
	p.Consume(strings.NewReader(stream))
	st := p.State()
	if st.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (malformed lines skipped)", st.Turns)
	}
	if st.ChangesSummary != "ok" {
		t.Errorf("ChangesSummary = %q, want ok", st.ChangesSummary)
	}
}
