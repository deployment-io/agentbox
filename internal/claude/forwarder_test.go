package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
)

type captureSink struct {
	chunks      []string
	finals      []string
	specs       []agent.SpecSnapshot
	suggestions []agent.RepoSuggestion
	turnEnds    int
}

func (s *captureSink) ForwardChunk(c agent.AssistantChunk) error {
	s.chunks = append(s.chunks, c.Text)
	return nil
}
func (s *captureSink) ForwardFinal(m agent.AssistantMessage) error {
	s.finals = append(s.finals, m.Text)
	return nil
}
func (s *captureSink) ForwardSpecUpdate(sp agent.SpecSnapshot) error {
	s.specs = append(s.specs, sp)
	return nil
}
func (s *captureSink) ForwardRepoSuggestion(rs agent.RepoSuggestion) error {
	s.suggestions = append(s.suggestions, rs)
	return nil
}
func (s *captureSink) ForwardTurnEnd() error {
	s.turnEnds++
	return nil
}

func assistantLine(text string) string {
	env := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, _ := json.Marshal(env)
	return string(b)
}

func partialLine(text string) string {
	env := map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": text},
		},
	}
	b, _ := json.Marshal(env)
	return string(b)
}

func TestForwarder_CompleteMessage(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(assistantLine("Hello there")))

	if len(sink.chunks) != 1 || sink.chunks[0] != "Hello there" {
		t.Errorf("chunks = %v, want [Hello there]", sink.chunks)
	}
	if len(sink.finals) != 1 || sink.finals[0] != "Hello there" {
		t.Errorf("finals = %v, want [Hello there]", sink.finals)
	}
}

func TestForwarder_StripsSpecBlock(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	text := "Here is the plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```"
	fwd.handleLine([]byte(assistantLine(text)))

	if len(sink.specs) != 1 {
		t.Fatalf("expected 1 spec update, got %d", len(sink.specs))
	}
	if sink.specs[0].Goal != "G" {
		t.Errorf("spec goal = %q, want G", sink.specs[0].Goal)
	}
	if len(sink.finals) != 1 || strings.Contains(sink.finals[0], "task-spec") {
		t.Errorf("final should have the spec block stripped: %v", sink.finals)
	}
	if !strings.Contains(sink.finals[0], "Here is the plan.") {
		t.Errorf("final should keep prose: %v", sink.finals)
	}
}

// A single assistant message carrying BOTH machine-only blocks forwards both
// payloads and produces display text with neither.
func TestForwarder_StripsBothBlocks(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	text := "Here is the plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```\n\n" +
		"<repo-suggestion>\n" +
		`{"repositories":[{"name":"deployment-io/kit","reason":"the Session model lives here","confidence":"high"}]}` +
		"\n</repo-suggestion>"
	fwd.handleLine([]byte(assistantLine(text)))

	if len(sink.specs) != 1 || sink.specs[0].Goal != "G" {
		t.Errorf("specs = %v, want one with goal G", sink.specs)
	}
	if len(sink.suggestions) != 1 || len(sink.suggestions[0].Repositories) != 1 {
		t.Fatalf("suggestions = %+v, want one naming a repository", sink.suggestions)
	}
	r := sink.suggestions[0].Repositories[0]
	if r.Name != "deployment-io/kit" || r.Reason != "the Session model lives here" || r.Confidence != "high" {
		t.Errorf("suggested repo = %+v", r)
	}
	if len(sink.finals) != 1 || sink.finals[0] != "Here is the plan." {
		t.Errorf("display text should be the prose alone: %v", sink.finals)
	}
}

// A suggestion-only message forwards the suggestion and no chat text, matching
// how a spec-only message already behaves.
func TestForwarder_SuggestionOnlyMessage(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(assistantLine("<repo-suggestion>\n" +
		`{"repositories":[{"name":"owner/repo","reason":"needed"}]}` + "\n</repo-suggestion>")))

	if len(sink.suggestions) != 1 {
		t.Fatalf("suggestions = %+v, want 1", sink.suggestions)
	}
	if len(sink.chunks) != 0 || len(sink.finals) != 0 {
		t.Errorf("a suggestion-only message must forward no chat text: chunks=%v finals=%v", sink.chunks, sink.finals)
	}
}

// The task-spec path is untouched by the new block: a message with only a spec
// forwards no suggestion.
func TestForwarder_SpecOnlyForwardsNoSuggestion(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(assistantLine("Plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```")))

	if len(sink.specs) != 1 || len(sink.suggestions) != 0 {
		t.Errorf("specs = %v, suggestions = %+v", sink.specs, sink.suggestions)
	}
}

func TestForwarder_PartialDeltasThenComplete(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(partialLine("Hel")))
	fwd.handleLine([]byte(partialLine("lo")))
	fwd.handleLine([]byte(assistantLine("Hello")))

	if strings.Join(sink.chunks, "") != "Hello" {
		t.Errorf("streamed chunks joined = %q, want Hello (%v)", strings.Join(sink.chunks, ""), sink.chunks)
	}
	if len(sink.chunks) != 2 {
		t.Errorf("expected 2 partial chunks (no duplicate for the completed message), got %d: %v", len(sink.chunks), sink.chunks)
	}
	if len(sink.finals) != 1 || sink.finals[0] != "Hello" {
		t.Errorf("finals = %v, want [Hello]", sink.finals)
	}
}

func TestForwarder_IgnoresNonJSON(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte("npm install output, not stream-json"))
	fwd.handleLine([]byte(assistantLine("real message")))

	if len(sink.finals) != 1 || sink.finals[0] != "real message" {
		t.Errorf("non-JSON noise should be ignored; finals = %v", sink.finals)
	}
}

func resultLine() string {
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": "Hello"})
	return string(b)
}

func errorResultLine(subtype, text string, turns int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": subtype, "is_error": true, "result": text, "num_turns": turns,
	})
	return string(b)
}

func TestForwarder_ResultEmitsTurnEnd(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(assistantLine("narration")))
	fwd.handleLine([]byte(assistantLine("the answer")))
	fwd.handleLine([]byte(resultLine()))

	if sink.turnEnds != 1 {
		t.Errorf("turnEnds = %d, want 1 (one per result event)", sink.turnEnds)
	}
	if len(sink.finals) != 2 {
		t.Errorf("finals = %v, want both turn messages before the boundary", sink.finals)
	}
}

func TestForwarder_ResultResetsPartialStreamState(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	// Partial deltas with no completed "assistant" event before the turn ends
	// (e.g. the completed event carried only a spec block). The result must
	// reset the streamed flag, or the NEXT turn's completed message would be
	// treated as already-streamed and its chunk dropped.
	fwd.handleLine([]byte(partialLine("orphan")))
	fwd.handleLine([]byte(resultLine()))
	fwd.handleLine([]byte(assistantLine("next turn")))

	want := []string{"orphan", "next turn"}
	if strings.Join(sink.chunks, "|") != strings.Join(want, "|") {
		t.Errorf("chunks = %v, want %v", sink.chunks, want)
	}
}

// A successful result forwards no chat text of its own — its text duplicates
// the last assistant message. Both the modern shape (subtype=success) and the
// bare shape (no subtype, no is_error) count as success.
func TestForwarder_SuccessfulResultForwardsNoText(t *testing.T) {
	for name, line := range map[string]string{
		"subtype success": resultLine(),
		"no subtype":      `{"type":"result","result":"Hello"}`,
	} {
		t.Run(name, func(t *testing.T) {
			sink := &captureSink{}
			fwd := newChunkForwarder(sink)
			fwd.handleLine([]byte(assistantLine("the answer")))
			fwd.handleLine([]byte(line))

			if len(sink.finals) != 1 || len(sink.chunks) != 1 {
				t.Errorf("a successful result must add no chat text: chunks=%v finals=%v", sink.chunks, sink.finals)
			}
			if sink.turnEnds != 1 {
				t.Errorf("turnEnds = %d, want 1", sink.turnEnds)
			}
		})
	}
}

// An errored result is chat-visible: the reason is forwarded as an assistant
// message (chunk + final, like any other) BEFORE the turn boundary, so the
// user sees why the composer re-opened without an answer.
func TestForwarder_ErroredResultIsChatVisible(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "max turns names the count and the interactive remedy",
			line:         errorResultLine("error_max_turns", "", 26),
			wantContains: []string{"turn limit", "26 turns", "send a message to continue"},
			wantAbsent:   []string{"max_turns"}, // the batch remedy is meaningless in a session
		},
		{
			name:         "max budget names the budget",
			line:         errorResultLine("error_max_budget_usd", "", 3),
			wantContains: []string{"spending budget"},
			wantAbsent:   []string{"max_budget_usd", "send a message"},
		},
		{
			name:         "execution error names the class and quotes the result text",
			line:         errorResultLine("error_during_execution", "Tool Bash timed out", 4),
			wantContains: []string{"turn failed", "error_during_execution", "Tool Bash timed out"},
		},
		{
			name:         "execution error with no result text still explains",
			line:         errorResultLine("error_during_execution", "", 0),
			wantContains: []string{"turn failed", "error_during_execution"},
		},
		{
			name:         "is_error with a success subtype is still a failure",
			line:         `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 500"}`,
			wantContains: []string{"turn failed", "API Error: 500"},
			wantAbsent:   []string{"(success)"},
		},
		{
			name:         "unknown non-success subtype without is_error is a failure",
			line:         `{"type":"result","subtype":"error_something_new","result":""}`,
			wantContains: []string{"turn failed", "error_something_new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &captureSink{}
			fwd := newChunkForwarder(sink)
			fwd.handleLine([]byte(assistantLine("working on it")))
			fwd.handleLine([]byte(tt.line))

			if len(sink.finals) != 2 {
				t.Fatalf("finals = %v, want the turn's message then the failure explanation", sink.finals)
			}
			got := sink.finals[1]
			if sink.chunks[len(sink.chunks)-1] != got {
				t.Errorf("failure must be forwarded as chunk + final; chunks=%v final=%q", sink.chunks, got)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("message %q missing %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("message %q must not contain %q", got, absent)
				}
			}
			if sink.turnEnds != 1 {
				t.Errorf("turnEnds = %d, want 1 (the failure is still a turn boundary)", sink.turnEnds)
			}
		})
	}
}

// The failure message comes before the boundary, and the next turn is
// unaffected: streamed state is reset and its message forwards normally.
func TestForwarder_ErroredResultOrderingAndNextTurn(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(partialLine("orphan")))
	fwd.handleLine([]byte(errorResultLine("error_during_execution", "boom", 1)))
	if sink.turnEnds != 1 || len(sink.finals) != 1 {
		t.Fatalf("after the failed turn: turnEnds=%d finals=%v", sink.turnEnds, sink.finals)
	}
	fwd.handleLine([]byte(assistantLine("next turn")))

	if len(sink.finals) != 2 || sink.finals[1] != "next turn" {
		t.Errorf("finals = %v, want the failure then the next turn's message", sink.finals)
	}
	if sink.chunks[len(sink.chunks)-1] != "next turn" {
		t.Errorf("next turn's chunk was dropped as already-streamed: chunks=%v", sink.chunks)
	}
}

// A long result excerpt is collapsed to one line and truncated, so a stack
// trace in the result text doesn't become the chat message.
func TestForwarder_ErroredResultExcerptIsBounded(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	long := strings.Repeat("line of error output\n", 50)
	fwd.handleLine([]byte(errorResultLine("error_during_execution", long, 1)))

	if len(sink.finals) != 1 {
		t.Fatalf("finals = %v", sink.finals)
	}
	got := sink.finals[0]
	if strings.Contains(got, "\n") {
		t.Errorf("excerpt must be one line: %q", got)
	}
	if len(got) > resultExcerptLen+80 {
		t.Errorf("excerpt not truncated: len=%d %q", len(got), got)
	}
}

// Claude Code reports an API error as an assistant message and repeats the
// text on the failed result. The explanation must not quote what the user has
// just read — but a later turn failing with the same text still quotes it.
func TestForwarder_ErroredResultDoesNotRepeatShownMessage(t *testing.T) {
	sink := &captureSink{}
	fwd := newChunkForwarder(sink)
	fwd.handleLine([]byte(assistantLine("API Error: 500 overloaded")))
	fwd.handleLine([]byte(errorResultLine("error_during_execution", "API Error: 500 overloaded", 1)))

	if len(sink.finals) != 2 {
		t.Fatalf("finals = %v, want the error message then the explanation", sink.finals)
	}
	if got := sink.finals[1]; strings.Contains(got, "API Error") || !strings.Contains(got, "error_during_execution") {
		t.Errorf("explanation = %q, want the class without the repeated text", got)
	}

	fwd.handleLine([]byte(errorResultLine("error_during_execution", "API Error: 500 overloaded", 1)))
	if got := sink.finals[len(sink.finals)-1]; !strings.Contains(got, "API Error: 500 overloaded") {
		t.Errorf("next turn's explanation = %q, want the result text (nothing was shown this turn)", got)
	}
}
