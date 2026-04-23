package claude

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/result"
)

func TestStreamParser_ResultEventPopulatesState(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"system","session_id":"abc"}
{"type":"result","result":"Added AuthToken type to kit/auth.","num_turns":4,"is_error":false,"usage":{"input_tokens":1200,"output_tokens":350,"cache_read_input_tokens":8000}}
`))

	state := p.State()
	if state.ChangesSummary != "Added AuthToken type to kit/auth." {
		t.Errorf("ChangesSummary = %q", state.ChangesSummary)
	}
	if state.Turns != 4 {
		t.Errorf("Turns = %d, want 4", state.Turns)
	}
	if state.IsError {
		t.Error("IsError should be false")
	}
	want := result.TokenUsage{InputTokens: 1200, OutputTokens: 350, CacheReadTokens: 8000}
	if state.TokenUsage != want {
		t.Errorf("TokenUsage = %+v, want %+v", state.TokenUsage, want)
	}
}

func TestStreamParser_AssistantToolUseTracksFileEdits(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll edit two files."},{"type":"tool_use","name":"Edit","input":{"file_path":"/work/kit/auth.go","old_string":"x","new_string":"y"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/work/kit/auth_test.go","content":"package auth"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"MultiEdit","input":{"file_path":"/work/app/main.go","edits":[]}}]}}
`))

	files := p.State().FilesChanged
	want := []string{"/work/app/main.go", "/work/kit/auth.go", "/work/kit/auth_test.go"}
	if !equalStrings(files, want) {
		t.Errorf("FilesChanged = %v, want %v", files, want)
	}
}

func TestStreamParser_DedupsRepeatedEdits(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"1"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"2"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"3"}}]}}
`))

	files := p.State().FilesChanged
	if len(files) != 1 || files[0] != "/work/a.go" {
		t.Errorf("expected a single deduped entry, got %v", files)
	}
}

func TestStreamParser_IgnoresNonFileModifyingTools(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/work/a.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"func"}}]}}
`))

	if len(p.State().FilesChanged) != 0 {
		t.Errorf("read/bash/grep should not populate FilesChanged: got %v", p.State().FilesChanged)
	}
}

func TestStreamParser_TolerantOfMalformedLines(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
this is not json
{broken: "object"
{"type":"result","result":"survived malformed lines","num_turns":1}
also not json
`))

	state := p.State()
	if state.ChangesSummary != "survived malformed lines" {
		t.Errorf("parser should have recovered: ChangesSummary = %q", state.ChangesSummary)
	}
	if state.Turns != 1 {
		t.Errorf("Turns = %d, want 1", state.Turns)
	}
}

func TestStreamParser_EmptyStream(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(""))

	state := p.State()
	if state.ChangesSummary != "" || state.Turns != 0 || len(state.FilesChanged) != 0 {
		t.Errorf("empty stream should leave all fields at zero: %+v", state)
	}
}

func TestStreamParser_IsAuthFailure(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		isAuth bool
	}{
		{"success is never auth failure", `{"type":"result","result":"done","is_error":false}`, false},
		{"auth subtype", `{"type":"result","result":"401 Unauthorized","is_error":true,"subtype":"error_auth"}`, true},
		{"api_key subtype", `{"type":"result","result":"Invalid request","is_error":true,"subtype":"invalid_api_key"}`, true},
		{"unauthorized keyword in summary", `{"type":"result","result":"Request failed: Unauthorized","is_error":true,"subtype":"error_api"}`, true},
		{"rate limit keyword", `{"type":"result","result":"rate limit exceeded","is_error":true}`, true},
		{"bedrock model-not-enabled", `{"type":"result","result":"Anthropic model not enabled in region us-east-2","is_error":true}`, true},
		{"generic execution error is not auth", `{"type":"result","result":"The code failed to compile","is_error":true}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newStreamParser()
			p.Consume(strings.NewReader(tt.input))
			if got := p.State().IsAuthFailure; got != tt.isAuth {
				t.Errorf("IsAuthFailure = %v, want %v", got, tt.isAuth)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
