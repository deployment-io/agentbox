package agent

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/result"
)

func TestStreamParser_ResultEventPopulatesOutcome(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"system","session_id":"abc"}
{"type":"result","result":"Added AuthToken type to kit/auth.","num_turns":4,"is_error":false,"usage":{"input_tokens":1200,"output_tokens":350,"cache_read_input_tokens":8000}}
`))

	if p.changesSummary != "Added AuthToken type to kit/auth." {
		t.Errorf("changesSummary = %q", p.changesSummary)
	}
	if p.turns != 4 {
		t.Errorf("turns = %d, want 4", p.turns)
	}
	if p.isError {
		t.Error("isError should be false")
	}
	want := result.TokenUsage{InputTokens: 1200, OutputTokens: 350, CacheReadTokens: 8000}
	if p.usage != want {
		t.Errorf("usage = %+v, want %+v", p.usage, want)
	}
}

func TestStreamParser_AssistantToolUseTracksFileEdits(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll edit two files."},{"type":"tool_use","name":"Edit","input":{"file_path":"/work/kit/auth.go","old_string":"x","new_string":"y"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/work/kit/auth_test.go","content":"package auth"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"MultiEdit","input":{"file_path":"/work/app/main.go","edits":[]}}]}}
`))

	files := p.filesChangedSorted()
	want := []string{"/work/app/main.go", "/work/kit/auth.go", "/work/kit/auth_test.go"}
	if !equalStrings(files, want) {
		t.Errorf("filesChanged = %v, want %v", files, want)
	}
}

func TestStreamParser_DedupsRepeatedEdits(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"1"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"2"}}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/work/a.go","old_string":"3"}}]}}
`))

	files := p.filesChangedSorted()
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

	if len(p.filesChangedSorted()) != 0 {
		t.Errorf("read/bash/grep should not populate filesChanged: got %v", p.filesChangedSorted())
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

	if p.changesSummary != "survived malformed lines" {
		t.Errorf("parser should have recovered: changesSummary = %q", p.changesSummary)
	}
	if p.turns != 1 {
		t.Errorf("turns = %d, want 1", p.turns)
	}
}

func TestStreamParser_EmptyStream(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(""))

	if p.changesSummary != "" || p.turns != 0 || len(p.filesChangedSorted()) != 0 {
		t.Errorf("empty stream should leave all fields at zero: %+v", p)
	}
}

func TestStreamParser_IsAuthFailureBySubtype(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		isAuth  bool
	}{
		{
			name:    "success is never auth failure",
			input:   `{"type":"result","result":"done","is_error":false}`,
			isAuth:  false,
		},
		{
			name:    "auth subtype",
			input:   `{"type":"result","result":"401 Unauthorized","is_error":true,"subtype":"error_auth"}`,
			isAuth:  true,
		},
		{
			name:    "api_key subtype",
			input:   `{"type":"result","result":"Invalid request","is_error":true,"subtype":"invalid_api_key"}`,
			isAuth:  true,
		},
		{
			name:    "unauthorized keyword in summary",
			input:   `{"type":"result","result":"Request failed: Unauthorized","is_error":true,"subtype":"error_api"}`,
			isAuth:  true,
		},
		{
			name:    "rate limit keyword",
			input:   `{"type":"result","result":"rate limit exceeded","is_error":true}`,
			isAuth:  true,
		},
		{
			name:    "bedrock model-not-enabled",
			input:   `{"type":"result","result":"Anthropic model not enabled in region us-east-2","is_error":true}`,
			isAuth:  true,
		},
		{
			name:    "generic execution error is not auth",
			input:   `{"type":"result","result":"The code failed to compile","is_error":true}`,
			isAuth:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newStreamParser()
			p.Consume(strings.NewReader(tt.input))
			if got := p.isAuthFailure(); got != tt.isAuth {
				t.Errorf("isAuthFailure() = %v, want %v (state: isError=%v, subtype=%q, summary=%q)",
					got, tt.isAuth, p.isError, p.errorSubtype, p.changesSummary)
			}
		})
	}
}

func TestHasAuthKeyword(t *testing.T) {
	positives := []string{
		"Invalid API key",
		"unauthorized access",
		"HTTP 401",
		"HTTP 429",
		"rate limit exceeded",
		"you have exceeded your quota",
		"request was throttled",
		"AccessDenied: cannot invoke model",
		"Model access not enabled in region",
	}
	for _, s := range positives {
		if !hasAuthKeyword(strings.ToLower(s)) {
			t.Errorf("hasAuthKeyword(%q) = false, want true", s)
		}
	}

	negatives := []string{
		"the code failed to compile",
		"no such file or directory",
		"syntax error on line 42",
		"",
	}
	for _, s := range negatives {
		if hasAuthKeyword(strings.ToLower(s)) {
			t.Errorf("hasAuthKeyword(%q) = true, want false", s)
		}
	}
}

// equalStrings reports whether two slices have the same elements in order.
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
