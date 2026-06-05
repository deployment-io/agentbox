package codex

import (
	"slices"
	"strings"
	"testing"
)

// TestParser_HappyPath drives a representative `codex exec --json` stream
// and pins the full ParsedState mapping. The event/item shapes follow the
// documented schema; the file_change shape is best-effort (see parser.go).
func TestParser_HappyPath(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1","model":"gpt-5.5"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"main.go","kind":"update"},{"path":"README.md","kind":"add"}]}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":50,"reasoning_output_tokens":10}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Did the thing.\n\n<verify>{\"ran\":true,\"passed\":true,\"command\":\"go build ./...\"}</verify>\n\n<pr_title>Add the thing</pr_title>"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":2000,"cached_input_tokens":0,"output_tokens":80,"reasoning_output_tokens":20}}`,
	}, "\n")

	p := newJSONLParser()
	p.Consume(strings.NewReader(stream))
	st := p.State()

	if st.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want gpt-5.5", st.Model)
	}
	if st.Turns != 2 {
		t.Errorf("Turns = %d, want 2", st.Turns)
	}
	if st.TokenUsage.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", st.TokenUsage.InputTokens)
	}
	if st.TokenUsage.OutputTokens != 130 { // 50 + 80; reasoning is already inside output_tokens, not added on top
		t.Errorf("OutputTokens = %d, want 130", st.TokenUsage.OutputTokens)
	}
	if st.TokenUsage.CacheReadTokens != 200 {
		t.Errorf("CacheReadTokens = %d, want 200", st.TokenUsage.CacheReadTokens)
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

// TestParser_IncrementalTurnsVisibleMidStream pins the contract the
// agentbox limit watcher depends on: turns and usage are observable via
// State() BEFORE the stream ends (unlike the claude parser, which only
// populates them on its final result event).
func TestParser_IncrementalTurnsVisibleMidStream(t *testing.T) {
	p := newJSONLParser()
	if got := p.State().Turns; got != 0 {
		t.Fatalf("Turns = %d before any event, want 0", got)
	}
	p.processLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`))
	if got := p.State().Turns; got != 1 {
		t.Errorf("Turns = %d after 1 turn.completed, want 1", got)
	}
	p.processLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}`))
	if got := p.State().Turns; got != 2 {
		t.Errorf("Turns = %d after 2 turn.completed, want 2", got)
	}
	if got := p.State().TokenUsage.OutputTokens; got != 10 {
		t.Errorf("OutputTokens = %d, want 10", got)
	}
}

func TestParser_Error(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"turn.failed nested error", `{"type":"turn.failed","error":{"message":"rate limit exceeded"}}`},
		{"error flat message", `{"type":"error","message":"something broke"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newJSONLParser()
			p.Consume(strings.NewReader(tc.line))
			st := p.State()
			if !st.IsError {
				t.Error("IsError should be true")
			}
			if st.FailureReason == "" {
				t.Error("FailureReason should carry the error message")
			}
		})
	}
}

func TestParser_MalformedLinesSkipped(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":1}}`,
		`{"type":`, // truncated json
		`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
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

func TestParser_AuthFailureClassified(t *testing.T) {
	p := newJSONLParser()
	p.Consume(strings.NewReader(`{"type":"turn.failed","error":{"message":"401 invalid api key"}}`))
	if !p.State().IsAuthFailure {
		t.Error("IsAuthFailure should be true for an api-key/401 error")
	}
}
