package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
)

type captureSink struct {
	chunks   []string
	finals   []string
	specs    []agent.SpecSnapshot
	turnEnds int
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
	b, _ := json.Marshal(map[string]any{"type": "result", "result": "Hello"})
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
