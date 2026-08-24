package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/result"
)

func TestStreamParser_ResultEventPopulatesState(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"system","session_id":"abc"}
{"type":"result","result":"Added AuthToken type to kit/auth.","num_turns":4,"is_error":false,"usage":{"input_tokens":1200,"output_tokens":350,"cache_read_input_tokens":8000,"cache_creation_input_tokens":1500}}
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
	want := result.TokenUsage{InputTokens: 1200, OutputTokens: 350, CacheReadTokens: 8000, CacheCreationTokens: 1500}
	if state.TokenUsage != want {
		t.Errorf("TokenUsage = %+v, want %+v", state.TokenUsage, want)
	}
}

func TestStreamParser_CapturesTotalCostUSD(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"done","num_turns":2,"is_error":false,"total_cost_usd":0.0421,"usage":{"input_tokens":1200,"output_tokens":350}}
`))
	got := p.State().CostUSD
	if got == nil {
		t.Fatal("CostUSD should be captured from the result event's total_cost_usd, got nil")
	}
	if *got != 0.0421 {
		t.Errorf("CostUSD = %v, want 0.0421", *got)
	}
}

func TestStreamParser_CostNilWhenNotReported(t *testing.T) {
	// Streams without total_cost_usd (e.g. Codex, or an interrupted Claude
	// run) must leave CostUSD nil so downstream can tell "no cost reported"
	// apart from a genuine $0.00.
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"done","num_turns":1,"is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}
`))
	if got := p.State().CostUSD; got != nil {
		t.Errorf("CostUSD should be nil when total_cost_usd is absent, got %v", *got)
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

func TestStreamParser_CapturesModelFromSystemInit(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"system","subtype":"init","model":"claude-opus-4-7","session_id":"abc"}
{"type":"result","result":"done","num_turns":1,"is_error":false}
`))

	if got := p.State().Model; got != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7 (from system.init)", got)
	}
}

func TestStreamParser_ModelFallsBackToAssistantMessage(t *testing.T) {
	// No system.init event — verifies the assistant-message fallback
	// path still captures the model. In practice Claude Code always
	// emits init first, but the fallback guards against truncated
	// streams where init was dropped.
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}]}}
{"type":"result","result":"done","num_turns":1,"is_error":false}
`))

	if got := p.State().Model; got != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6 (from assistant message)", got)
	}
}

func TestStreamParser_LastSeenModelWins(t *testing.T) {
	// Mid-run rerouting is rare but possible; assert the parser
	// reflects the most recent model rather than the first.
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"x"}]}}
`))

	if got := p.State().Model; got != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want last-seen claude-sonnet-4-6", got)
	}
}

func TestStreamParser_ModelEmptyWhenNeverReported(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"done","num_turns":1,"is_error":false}
`))

	if got := p.State().Model; got != "" {
		t.Errorf("Model = %q, want empty when never reported", got)
	}
}

// TestStreamParser_PRTitleTrailerExtracted pins the Bug 2 fix: the
// agent's system prompt instructs it to end the final message with a
// <pr_title>...</pr_title> block; the parser extracts the inner text
// as a distinct field and strips the tag from changes_summary so the
// PR body lead-in doesn't carry the title marker.
func TestStreamParser_PRTitleTrailerExtracted(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"Reduced the Node heap cap so the dashboard build fits in a 2 GB CI worker.\n\n<pr_title>Tighten Node heap cap in dashboard build script</pr_title>","num_turns":3,"is_error":false}
`))

	state := p.State()
	if state.PRTitle != "Tighten Node heap cap in dashboard build script" {
		t.Errorf("PRTitle = %q, want %q", state.PRTitle, "Tighten Node heap cap in dashboard build script")
	}
	if strings.Contains(state.ChangesSummary, "<pr_title>") {
		t.Errorf("ChangesSummary should have the trailer stripped: %q", state.ChangesSummary)
	}
	if !strings.Contains(state.ChangesSummary, "Reduced the Node heap cap") {
		t.Errorf("ChangesSummary lost the body text: %q", state.ChangesSummary)
	}
	if strings.HasSuffix(state.ChangesSummary, "\n") || strings.HasSuffix(state.ChangesSummary, " ") {
		t.Errorf("ChangesSummary should be right-trimmed after trailer strip: %q", state.ChangesSummary)
	}
}

func TestStreamParser_PRTitleAbsentLeavesEmpty(t *testing.T) {
	// Defensive: if the agent ignored the system-prompt instruction
	// and emitted no <pr_title> tag, PRTitle stays empty and the full
	// result text becomes ChangesSummary.
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"Added AuthToken type to kit/auth.","num_turns":1,"is_error":false}
`))

	state := p.State()
	if state.PRTitle != "" {
		t.Errorf("PRTitle = %q, want empty when agent didn't emit trailer", state.PRTitle)
	}
	if state.ChangesSummary != "Added AuthToken type to kit/auth." {
		t.Errorf("ChangesSummary = %q, want full result text", state.ChangesSummary)
	}
}

func TestStreamParser_PRTitleTrimsInnerWhitespace(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"body\n\n<pr_title>   Padded title   </pr_title>","num_turns":1,"is_error":false}
`))

	if got := p.State().PRTitle; got != "Padded title" {
		t.Errorf("PRTitle = %q, want %q", got, "Padded title")
	}
}

func TestStreamParser_PRTitleUnclosedTagFallsBack(t *testing.T) {
	// Malformed trailer (missing closing tag) — don't crash, don't
	// extract, leave the full text as changes_summary so the runner's
	// fallback path can still produce a sensible commit/PR.
	p := newStreamParser()
	p.Consume(strings.NewReader(`
{"type":"result","result":"body text <pr_title>oops never closed","num_turns":1,"is_error":false}
`))

	state := p.State()
	if state.PRTitle != "" {
		t.Errorf("PRTitle = %q, want empty for malformed trailer", state.PRTitle)
	}
	if !strings.Contains(state.ChangesSummary, "<pr_title>oops never closed") {
		t.Errorf("ChangesSummary should retain the malformed text: %q", state.ChangesSummary)
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

func TestStreamParser_FailureReason(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantContains string // "" means FailureReason must be empty
	}{
		{"max turns with count names the count and remedy", `{"type":"result","result":"","is_error":true,"subtype":"error_max_turns","num_turns":26}`, "after 26 turns"},
		{"max turns with count mentions max_turns", `{"type":"result","result":"","is_error":true,"subtype":"error_max_turns","num_turns":26}`, "max_turns"},
		{"max turns without count still explains", `{"type":"result","result":"","is_error":true,"subtype":"error_max_turns"}`, "turn limit"},
		{"success has no failure reason", `{"type":"result","result":"done","is_error":false,"subtype":"success"}`, ""},
		{"other error subtype falls back (no reason)", `{"type":"result","result":"boom","is_error":true,"subtype":"error_during_execution"}`, ""},
		{"error without subtype falls back (no reason)", `{"type":"result","result":"boom","is_error":true}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newStreamParser()
			p.Consume(strings.NewReader(tt.input))
			got := p.State().FailureReason
			if tt.wantContains == "" {
				if got != "" {
					t.Errorf("FailureReason = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("FailureReason = %q, want substring %q", got, tt.wantContains)
			}
		})
	}
}

// The denial a live Opus 5 run produced, verbatim in shape: a 403 model-access
// error wearing an authentication costume, closed by AWS's suggestion to
// contact sales. Every case below is a variation on this string.
const bedrockDenialSummary = `Failed to authenticate. API Error: 403 anthropic.claude-opus-5 is not available for this account. You can explore other available models on Amazon Bedrock. For additional access options, contact AWS Sales at https://aws.amazon.com/contact-us/sales-support/`

func bedrockDenialEvent(summary string) string {
	b, _ := json.Marshal(summary)
	return `{"type":"result","result":` + string(b) + `,"is_error":true,"subtype":"error_during_execution"}`
}

// A Bedrock model-access denial must come out as an instruction, because Claude
// Code's own words mislead twice: "Failed to authenticate" (authentication
// succeeded — the model grant was missing) and "contact AWS Sales" (the fix is
// the model-access console page). This is the DEFAULT agent, so this is the
// error a new Bedrock org is most likely to meet first.
func TestStreamParser_BedrockAccessDenialBecomesAnInstruction(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "eu-west-1")

	p := newStreamParser()
	p.Consume(strings.NewReader(bedrockDenialEvent(bedrockDenialSummary)))
	got := p.State().FailureReason

	for _, want := range []string{
		"is not available for this account", // AWS's own diagnosis survives
		"enable model access",               // the actual remedy
		"eu-west-1",                         // in the right region
		"direct provider",                   // and the alternative
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FailureReason = %q, missing %q", got, want)
		}
	}
	// The false diagnosis must not LEAD. It sent a real debugging session to
	// check credentials that were working fine.
	if strings.HasPrefix(got, "Failed to authenticate") {
		t.Errorf("FailureReason = %q; still leads with the authentication misdiagnosis", got)
	}
}

// Each gate on its own must suppress the hint — every one of these is a real
// way of sending someone to the wrong console page.
func TestStreamParser_BedrockAccessHintGates(t *testing.T) {
	tests := []struct {
		name    string
		bedrock string
		summary string
	}{
		// Anthropic Direct can phrase a 403 similarly; the AWS console cannot
		// fix a model AWS never served.
		{"not in bedrock mode", "", bedrockDenialSummary},
		// The sentence without a 403 could be an echo inside an unrelated
		// failure — a tool result, a quoted log line.
		{"sentence without a 403", "1", "the model is not available for this account (see logs)"},
		// 403 without the sentence is IAM or a bad signature, which "enable
		// model access" does not fix.
		{"403 without the sentence", "1", "Failed to authenticate. API Error: 403 AccessDeniedException: not authorized to perform bedrock:InvokeModel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", tt.bedrock)
			t.Setenv("AWS_REGION", "eu-west-1")
			p := newStreamParser()
			p.Consume(strings.NewReader(bedrockDenialEvent(tt.summary)))
			if got := p.State().FailureReason; strings.Contains(got, "enable model access") {
				t.Errorf("FailureReason = %q; the hint fired without its gates", got)
			}
		})
	}
}

// No region in the env still produces usable advice rather than a hole in the
// sentence.
func TestStreamParser_BedrockAccessHintWithoutRegion(t *testing.T) {
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "")
	p := newStreamParser()
	p.Consume(strings.NewReader(bedrockDenialEvent(bedrockDenialSummary)))
	if got := p.State().FailureReason; !strings.Contains(got, "this region") {
		t.Errorf("FailureReason = %q, want the region placeholder %q", got, "this region")
	}
}

func TestStreamParser_ErrorSubtypeExposedOnlyOnError(t *testing.T) {
	p := newStreamParser()
	p.Consume(strings.NewReader(`{"type":"result","result":"boom","is_error":true,"subtype":"error_during_execution"}`))
	if got := p.State().ErrorSubtype; got != "error_during_execution" {
		t.Errorf("ErrorSubtype = %q, want error_during_execution", got)
	}

	ok := newStreamParser()
	ok.Consume(strings.NewReader(`{"type":"result","result":"done","is_error":false,"subtype":"success"}`))
	if got := ok.State().ErrorSubtype; got != "" {
		t.Errorf("ErrorSubtype = %q, want empty on success", got)
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
